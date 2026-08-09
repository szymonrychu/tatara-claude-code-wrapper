package session_test

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

// testPTY is the ptyWriter shape as seen from outside the package, so one
// helper can take either fakePTY (concatenating) or splitPTY (per-write).
type testPTY interface {
	io.Writer
	Close() error
}

// newInterruptMgr builds a manager with a caller-supplied registry (so counters
// can be asserted) and a frozen clock, matching the other session tests.
func newInterruptMgr(t *testing.T, w testPTY, turnTimeout time.Duration) (*session.Manager, *turn.Store, *prometheus.Registry) {
	t.Helper()
	store := turn.NewStore()
	reg := prometheus.NewRegistry()
	var idMu sync.Mutex
	n := 0
	m := session.New(
		session.Config{TurnTimeout: turnTimeout, SubmitDelay: time.Millisecond, SubmitSeq: session.DefaultSubmitSeq},
		store, metrics.New(reg),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() time.Time { return time.Unix(100, 0) },
		func() string {
			idMu.Lock()
			defer idMu.Unlock()
			n++
			return "turn-" + strconv.Itoa(n)
		})
	m.SetWriterForTest(w)
	return m, store, reg
}

// counterValue reads a label-free counter, or the value for one `result` label
// on a CounterVec. Returns 0 when the series has not been touched.
func counterValue(t *testing.T, reg *prometheus.Registry, name, resultLabel string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, mm := range mf.GetMetric() {
			if resultLabel == "" {
				return mm.GetCounter().GetValue()
			}
			for _, lp := range mm.GetLabel() {
				if lp.GetValue() == resultLabel {
					return mm.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// fixtureLines reads a transcript fixture from testdata as its JSONL lines.
func fixtureLines(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(filepath.Join("testdata", name)))
	require.NoError(t, err)
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// liveTranscript writes lines to a fresh temp file and returns the path plus an
// append function, standing in for the CLI writing its conversation JSONL.
func liveTranscript(t *testing.T, lines []string) (path string, appendLine func(string)) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "conv.jsonl")
	write := func(l []string) {
		t.Helper()
		var b strings.Builder
		for _, ln := range l {
			b.WriteString(ln)
			b.WriteString("\n")
		}
		f, err := os.OpenFile(filepath.Join(dir, "conv.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		require.NoError(t, err)
		_, err = f.WriteString(b.String())
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}
	write(lines)
	return path, func(l string) { write([]string{l}) }
}

// TestInterrupt_ResolvesTurnAndNeverWedgesBusy is the load-bearing test of
// phase W4.
//
// An ESC'd turn produces no `end_turn`, no stop_reason and no Stop hook: every
// existing completion path in the manager keys on one of those, so nothing
// clears mgr.current and the session sits Busy for the rest of the pod's life.
// A wedge here is silent and permanent - the operator's own escalation is what
// sends the ESC, and the very next thing it does is POST /v1/messages for the
// handoff turn, which would then 409 forever. The pod would be stopped with a
// synthetic note in place of the agent's own, which is precisely the continuity
// loss the whole stall rework exists to fix.
//
// The test therefore drives the real sequence: a live turn, an ESC, the marker
// appearing in the transcript a moment later, and then asserts the two things
// that matter - the turn resolves as interrupted, and the session accepts the
// next turn.
func TestInterrupt_ResolvesTurnAndNeverWedgesBusy(t *testing.T) {
	lines := fixtureLines(t, "interrupted_turn.jsonl")
	marker := lines[len(lines)-1]
	path, appendLine := liveTranscript(t, lines[:len(lines)-1])

	fp := &fakePTY{}
	m, store, reg := newInterruptMgr(t, fp, time.Minute)
	m.SetTranscriptPathForTest(path)
	done := make(chan *turn.Record, 2)
	m.OnTurnDone = func(r *turn.Record) { done <- r }

	id, err := m.Submit("do the thing", "https://cb/x", false)
	require.NoError(t, err)
	require.Equal(t, "turn-1", id)
	require.Equal(t, session.Busy, m.Snapshot().State)

	turnID, err := m.Interrupt()
	require.NoError(t, err)
	require.Equal(t, "turn-1", turnID, "Interrupt reports which turn it cut short")
	require.Contains(t, string(fp.bytes()), session.InterruptSeq)

	// The ESC alone must not resolve anything: the turn is only over once the
	// CLI says so in the transcript. Resolving on the write would hand the
	// operator a completion for work that may still be running.
	time.Sleep(150 * time.Millisecond)
	rec, _ := store.Get("turn-1")
	require.Equal(t, turn.Running, rec.State, "the turn must stay in flight until the marker lands")

	// The CLI writes the interruption marker.
	appendLine(marker)

	select {
	case r := <-done:
		require.Equal(t, turn.Failed, r.State,
			"an interrupted turn did not finish its work; reporting Complete would let the operator advance a Task on it")
		require.Equal(t, turn.StopReasonInterrupted, r.StopReason)
		require.Equal(t, turn.ErrInterrupted, r.Error)
		require.Equal(t, "Starting sleep 1 of 5.", r.FinalText,
			"the partial output is the only record of what the agent was doing when it was cut off")
	case <-time.After(5 * time.Second):
		t.Fatal("interrupted turn never resolved: the session is wedged Busy, which is the exact failure this test exists to catch")
	}

	// THE anti-wedge assertions.
	snap := m.Snapshot()
	require.Equal(t, session.Ready, snap.State, "the session must be Ready for the operator's handoff turn")
	require.True(t, snap.StallSuspectedSince.IsZero(), "a resolved turn carries no stall marker")

	next, err := m.Submit("hand off please", "", false)
	require.NoError(t, err, "POST /v1/messages must work immediately after an interrupt; a 409 here means the pod can never hand off")
	require.Equal(t, "turn-2", next)

	require.Equal(t, float64(1), counterValue(t, reg, "ccw_turns_total", "interrupted"))
	require.Equal(t, float64(1), counterValue(t, reg, "ccw_interrupts_total", "ok"))
}

// TestInterrupt_WritesExactlyOneEscByte pins the wire format. ESC is the only
// input the CLI treats as a cancel; anything longer is a message, and a message
// waits for the next tool-call boundary - useless against a turn wedged inside
// one tool call, which is the case worth interrupting. Bracketed-paste framing
// or a trailing CR would turn the cancel into exactly that.
func TestInterrupt_WritesExactlyOneEscByte(t *testing.T) {
	w := &splitPTY{}
	m, _, _ := newInterruptMgr(t, w, time.Minute)

	_, err := m.Interrupt()
	require.NoError(t, err)

	writes, _ := w.snapshot()
	require.Equal(t, []string{"\x1b"}, writes,
		"exactly one write of exactly one ESC byte: no paste framing, no SubmitDelay, no submit keystroke")
	require.Equal(t, session.InterruptSeq, writes[0])
	require.Len(t, []byte(writes[0]), 1)
}

// TestInterrupt_IdleSessionIsANoOp: the operator can lose the race between its
// escalation and the turn finishing on its own. That must be harmless, not an
// error, and must not resolve or invent a turn.
func TestInterrupt_IdleSessionIsANoOp(t *testing.T) {
	w := &splitPTY{}
	m, store, _ := newInterruptMgr(t, w, time.Minute)

	turnID, err := m.Interrupt()
	require.NoError(t, err)
	require.Empty(t, turnID)
	require.Empty(t, store.List())
	require.Equal(t, session.Ready, m.Snapshot().State)
}

// TestInterrupt_UnavailableWithoutLivePTY maps the no-PTY cases to an error the
// handler turns into 503 (retryable), never 409.
func TestInterrupt_UnavailableWithoutLivePTY(t *testing.T) {
	for _, tc := range []struct {
		name string
		kill func(*session.Manager)
	}{
		{"dead", func(m *session.Manager) { m.SetDeadForTest() }},
		{"booting", func(m *session.Manager) { m.SetBootingForTest() }},
		{"stopping", func(m *session.Manager) { m.SetStoppingForTest() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &splitPTY{}
			m, _, reg := newInterruptMgr(t, w, time.Minute)
			tc.kill(m)

			_, err := m.Interrupt()
			require.ErrorIs(t, err, session.ErrInterruptUnavailable)
			writes, _ := w.snapshot()
			require.Empty(t, writes, "nothing may be written to a PTY that is gone")
			require.Equal(t, float64(1), counterValue(t, reg, "ccw_interrupts_total", "unavailable"))
		})
	}
}

// TestInterrupt_HistoricalMarkerDoesNotResolveTurn: a conversation that was
// interrupted earlier and carried on still holds the marker in its history. If
// WasInterrupted matched anywhere in the file, the very first poll after ANY
// interrupt would resolve the live turn instantly - including a turn the ESC
// never reached.
func TestInterrupt_HistoricalMarkerDoesNotResolveTurn(t *testing.T) {
	path, _ := liveTranscript(t, fixtureLines(t, "interrupted_then_resumed.jsonl"))

	fp := &fakePTY{}
	m, store, _ := newInterruptMgr(t, fp, time.Minute)
	m.SetTranscriptPathForTest(path)

	_, err := m.Submit("go", "", false)
	require.NoError(t, err)
	_, err = m.Interrupt()
	require.NoError(t, err)

	time.Sleep(300 * time.Millisecond)
	rec, _ := store.Get("turn-1")
	require.Equal(t, turn.Running, rec.State,
		"a marker in the conversation's history is not this turn's interruption")
	require.Equal(t, session.Busy, m.Snapshot().State)
}

// TestInactivityTimer_MarksStallSuspectedAndReArms is the other half of W4.
//
// The inactivity timer used to call failTimeout and kill the turn. It no longer
// does anything of the kind: the wrapper cannot tell a hang from productive
// work it cannot see (one subagent run silenced the parent transcript for 35
// minutes), so it reports and the operator decides. This test asserts the turn
// SURVIVES, the report is made, and the timer re-arms - a timer that fired once
// and stopped would go quiet exactly as the stall got serious.
func TestInactivityTimer_MarksStallSuspectedAndReArms(t *testing.T) {
	fp := &fakePTY{}
	m, store, reg := newInterruptMgr(t, fp, 40*time.Millisecond)
	done := make(chan *turn.Record, 1)
	m.OnTurnDone = func(r *turn.Record) { done <- r }

	_, err := m.Submit("a long quiet turn", "https://cb/x", false)
	require.NoError(t, err)

	// Silence for several TurnTimeout windows.
	time.Sleep(300 * time.Millisecond)

	select {
	case r := <-done:
		t.Fatalf("the wrapper failed a turn on inactivity (state=%s, err=%q); that judgement belongs to the operator now", r.State, r.Error)
	default:
	}

	rec, ok := store.Get("turn-1")
	require.True(t, ok)
	require.Equal(t, turn.Running, rec.State, "the turn must still be live")

	snap := m.Snapshot()
	require.Equal(t, session.Busy, snap.State)
	require.Equal(t, time.Unix(100, 0), snap.StallSuspectedSince,
		"the operator anchors its escalation on this stamp, and it is set once so it measures how long the silence has lasted")

	fired := counterValue(t, reg, "ccw_turn_stall_suspected_total", "")
	require.GreaterOrEqual(t, fired, float64(2),
		"the timer must re-arm; a single firing means the wrapper goes quiet exactly when the stall gets serious")

	// Genuine activity clears the stamp: a turn that came back to life is not
	// stalled, and a sticky stamp is a false positive the operator cannot
	// recover from.
	m.FireActivityForTest("turn-1")
	require.True(t, m.Snapshot().StallSuspectedSince.IsZero())

	rec, _ = store.Get("turn-1")
	require.Equal(t, turn.Running, rec.State)
}

// TestInactivityTimer_ProbeActivityDoesNotClearStallStamp: the probe's own
// transcript lines are excluded from activity (phase W3) precisely so probing a
// hung turn cannot keep it looking alive. The stall stamp must obey the same
// rule, or the operator's escalation would erase its own evidence every time it
// asked a question.
func TestInactivityTimer_ProbeActivityDoesNotClearStallStamp(t *testing.T) {
	fp := &fakePTY{}
	m, _, _ := newInterruptMgr(t, fp, 40*time.Millisecond)

	_, err := m.Submit("quiet", "", false)
	require.NoError(t, err)
	time.Sleep(120 * time.Millisecond)
	require.False(t, m.Snapshot().StallSuspectedSince.IsZero())

	m.FireProbeActivityForTest("turn-1")
	require.Equal(t, time.Unix(100, 0), m.Snapshot().StallSuspectedSince,
		"a probe is an observation; it must not change the state of the thing observed")
}
