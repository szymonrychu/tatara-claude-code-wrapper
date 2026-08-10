package session_test

import (
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/transcript"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

// splitPTY records each Write as its own entry (unlike fakePTY, which
// concatenates), so a test can assert the exact byte sequence AND the exact
// number of syscalls the probe makes.
type splitPTY struct {
	mu     sync.Mutex
	writes []string
	at     []time.Time
	err    error // returned by the Nth write, see failOn
	failOn int   // 1-based index of the write that fails; 0 = never
	n      int
}

func (s *splitPTY) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	if s.failOn != 0 && s.n == s.failOn {
		return 0, s.err
	}
	s.writes = append(s.writes, string(p))
	s.at = append(s.at, time.Now())
	return len(p), nil
}

func (s *splitPTY) Close() error { return nil }

func (s *splitPTY) snapshot() ([]string, []time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.writes...), append([]time.Time(nil), s.at...)
}

func newProbeMgr(t *testing.T, w *splitPTY, turnTimeout, submitDelay time.Duration) (*session.Manager, *turn.Store) {
	t.Helper()
	store := turn.NewStore()
	// A counter rather than a fixed list: the probe tests call Probe in an
	// unbounded loop, and a channel of pre-seeded ids would deadlock instead
	// of failing.
	var idMu sync.Mutex
	n := 0
	m := session.New(
		session.Config{TurnTimeout: turnTimeout, SubmitDelay: submitDelay, SubmitSeq: session.DefaultSubmitSeq},
		store, metrics.New(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() time.Time { return time.Unix(100, 0) },
		func() string {
			idMu.Lock()
			defer idMu.Unlock()
			n++
			return "id-" + strconv.Itoa(n)
		})
	m.SetWriterForTest(w)
	return m, store
}

// TestProbe_WritesExactSubmitSequence pins the bytes. The probe is only useful
// because the CLI treats it as an ordinary pasted message - the spike that
// verified mid-turn steering (CLI 2.1.201 and 2.1.226) used precisely this
// sequence, and any deviation (a missing paste-end, a submit fused into the
// same write, a dropped SubmitDelay) is a different experiment with no
// evidence behind it.
func TestProbe_WritesExactSubmitSequence(t *testing.T) {
	w := &splitPTY{}
	m, _ := newProbeMgr(t, w, time.Minute, 30*time.Millisecond)

	text := "Do not stop. Reply with exactly TATARA-ALIVE <one sentence>."
	id, err := m.Probe(text)
	require.NoError(t, err)
	require.NotEmpty(t, id, "a probe gets its own id, and never a turn slot")

	writes, at := w.snapshot()
	require.Equal(t, []string{
		session.DefaultSubmitSeq.PasteStart + text + session.DefaultSubmitSeq.PasteEnd,
		session.DefaultSubmitSeq.Submit,
	}, writes, "probe must write paste-start+text+paste-end, then the submit keystroke, as two separate writes")
	require.GreaterOrEqual(t, at[1].Sub(at[0]), 30*time.Millisecond,
		"the SubmitDelay between the paste and the CR is what makes the CLI accept the paste as one message")
}

// TestProbe_EmptyTextUsesDefault: the operator polls the same question every
// time and the answer detector keys on a literal marker, so the wrapper owns
// the default rather than trusting every caller to restate it.
func TestProbe_EmptyTextUsesDefault(t *testing.T) {
	w := &splitPTY{}
	m, _ := newProbeMgr(t, w, time.Minute, time.Millisecond)

	_, err := m.Probe("   ")
	require.NoError(t, err)

	writes, _ := w.snapshot()
	require.Equal(t,
		session.DefaultSubmitSeq.PasteStart+session.DefaultProbeText+session.DefaultSubmitSeq.PasteEnd,
		writes[0])
	require.Contains(t, session.DefaultProbeText, transcript.ProbeAnswerPrefix,
		"the default probe must ask for the exact literal the answer detector matches")
}

// TestProbe_DoesNotTouchTurnSlotAndNeverBusy is the core contract. The
// operator physically cannot reach a running agent through POST /v1/messages,
// which maps session.ErrBusy to 409 for the whole duration of a turn - so a
// probe that could also return ErrBusy would be useless.
func TestProbe_DoesNotTouchTurnSlotAndNeverBusy(t *testing.T) {
	w := &splitPTY{}
	m, store := newProbeMgr(t, w, time.Minute, time.Millisecond)

	turnID, err := m.Submit("do the work", "", false)
	require.NoError(t, err)
	require.Equal(t, session.Busy, m.Snapshot().State)

	// A probe mid-turn: accepted, twice, with a turn in flight throughout.
	for range 2 {
		_, perr := m.Probe("still there?")
		require.NoError(t, perr, "Probe must never refuse because a turn is in flight")
		require.NotErrorIs(t, perr, session.ErrBusy)
	}

	// The turn slot is untouched: still Busy, still the same turn, and an
	// ordinary Submit is still correctly refused.
	require.Equal(t, session.Busy, m.Snapshot().State)
	_, err = m.Submit("second turn", "", false)
	require.ErrorIs(t, err, session.ErrBusy, "the probe must not have released the turn slot")

	// And no turn record was created for either probe.
	require.Len(t, store.List(), 1, "a probe is not a turn and must not appear in the turn store")
	rec, ok := store.Get(turnID)
	require.True(t, ok)
	require.Equal(t, turn.Running, rec.State)
}

// TestProbe_DoesNotResetTurnTimer is the anti-regression for the whole point
// of the phase, at the session level: sending a probe must not extend the
// deadline of the turn being probed. If it did, an operator investigating a
// hung agent every few minutes would keep that agent alive forever, and the
// more suspicious a turn looked the longer it would survive.
func TestProbe_DoesNotResetTurnTimer(t *testing.T) {
	w := &splitPTY{}
	m, _ := newProbeMgr(t, w, 200*time.Millisecond, time.Millisecond)
	done := make(chan *turn.Record, 1)
	m.OnTurnDone = func(r *turn.Record) { done <- r }

	turnID, err := m.Submit("do the work", "", false)
	require.NoError(t, err)

	// Probe repeatedly across a window longer than TurnTimeout. If Probe (or
	// the probe's own transcript line) reset the inactivity timer, the turn
	// would never reach its deadline.
	deadline := time.After(500 * time.Millisecond)
	probing := true
	for probing {
		select {
		case <-deadline:
			probing = false
		default:
			_, _ = m.Probe("still there?")
			m.FireProbeActivityForTest(turnID)
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Since W4 tripping the deadline reports rather than kills, so the
	// observable is the stall stamp: if probe activity reset the timer it would
	// never be set, and the operator would never escalate on a turn it was
	// actively investigating.
	require.False(t, m.Snapshot().StallSuspectedSince.IsZero(),
		"probing kept the turn looking alive: the inactivity timer was reset by probe activity")
	select {
	case r := <-done:
		t.Fatalf("the inactivity timer resolved the turn (state=%s); it may only report", r.State)
	default:
	}
	_ = turnID
}

// TestProbeActivity_DoesNotAdvanceLastActivityAt: the exclusion covers
// LastActivityAt too, not just the local timer. The operator anchors its own
// stall deadline on that field, so leaving it in would move the bug one level
// up rather than fixing it.
func TestProbeActivity_DoesNotAdvanceLastActivityAt(t *testing.T) {
	var clkMu sync.Mutex
	clock := time.Unix(100, 0)
	now := func() time.Time {
		clkMu.Lock()
		defer clkMu.Unlock()
		return clock
	}
	store := turn.NewStore()
	ids := make(chan string, 4)
	ids <- "turn-1"
	ids <- "probe-1"
	m := session.New(
		session.Config{TurnTimeout: 10 * time.Second, SubmitDelay: time.Millisecond, SubmitSeq: session.DefaultSubmitSeq},
		store, metrics.New(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		now, func() string { return <-ids })
	m.SetWriterForTest(&splitPTY{})

	_, err := m.Submit("hi", "", false)
	require.NoError(t, err)
	require.Equal(t, time.Unix(100, 0), m.Snapshot().LastActivityAt)

	clkMu.Lock()
	clock = time.Unix(400, 0)
	clkMu.Unlock()

	m.FireProbeActivityForTest("turn-1")
	require.Equal(t, time.Unix(100, 0), m.Snapshot().LastActivityAt,
		"a probe-originated line must not look like agent progress")

	// A genuine line still does, so the exclusion is narrow rather than a
	// blanket disabling of the heartbeat.
	m.FireActivityForTest("turn-1")
	require.Equal(t, time.Unix(400, 0), m.Snapshot().LastActivityAt)
}

// TestProbe_SnapshotReportsPendingSince: a probe stuck in pending is the
// blocked-inside-one-long-tool-call diagnosis, and the stamp is how the
// operator sees how long it has been stuck.
func TestProbe_SnapshotReportsPendingSince(t *testing.T) {
	w := &splitPTY{}
	m, _ := newProbeMgr(t, w, time.Minute, time.Millisecond)
	require.True(t, m.Snapshot().ProbePendingSince.IsZero(), "no probe, no stamp")

	id, err := m.Probe("still there?")
	require.NoError(t, err)
	require.Equal(t, time.Unix(100, 0), m.Snapshot().ProbePendingSince)

	// Delivery clears it: the message reached the model, so whatever else is
	// wrong, the turn is reaching tool-call boundaries.
	m.ProbeTrackerForTest().NoteRemoval(time.Unix(130, 0))
	require.True(t, m.Snapshot().ProbePendingSince.IsZero())

	st, ok := m.ProbeStatus(id)
	require.True(t, ok)
	require.Equal(t, transcript.ProbeStateDelivered, st.State)
}

// TestProbe_UnavailableStates: no live PTY means 503, not 409. Distinguishing
// them matters to the operator - one is retryable, the other says "this pod
// will never answer you".
func TestProbe_UnavailableStates(t *testing.T) {
	tests := []struct {
		name  string
		setup func(m *session.Manager)
	}{
		{"booting", func(m *session.Manager) { m.SetBootingForTest() }},
		{"dead", func(m *session.Manager) { m.SetDeadForTest() }},
		{"stopping", func(m *session.Manager) { m.SetStoppingForTest() }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := &splitPTY{}
			m, _ := newProbeMgr(t, w, time.Minute, time.Millisecond)
			tc.setup(m)

			_, err := m.Probe("still there?")
			require.ErrorIs(t, err, session.ErrProbeUnavailable)
			require.NotErrorIs(t, err, session.ErrBusy)
			writes, _ := w.snapshot()
			require.Empty(t, writes)
		})
	}
}

// TestProbe_WriteFailureIsReportedAndNotTracked: a probe whose write failed
// was never sent, so it must not linger as "pending" and later be counted as
// a never_delivered outcome. ccw_probes_total{result="write_fail"} is the
// record of it.
func TestProbe_WriteFailureIsReportedAndNotTracked(t *testing.T) {
	w := &splitPTY{failOn: 2, err: io.ErrClosedPipe}
	m, _ := newProbeMgr(t, w, time.Minute, time.Millisecond)

	id, err := m.Probe("still there?")
	require.Error(t, err)
	require.Empty(t, id)
	require.True(t, m.Snapshot().ProbePendingSince.IsZero(),
		"a probe that was never written must not be reported as pending")
	_, ok := m.ProbeStatus("id-1")
	require.False(t, ok)
}
