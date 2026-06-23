package session_test

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

// resumeModeCount reads ccw_turn_resumes_total for a given (result, resume_mode).
func resumeModeCount(t *testing.T, reg *prometheus.Registry, result, mode string) float64 {
	t.Helper()
	mfs, _ := reg.Gather()
	for _, mf := range mfs {
		if mf.GetName() != "ccw_turn_resumes_total" {
			continue
		}
		for _, mm := range mf.GetMetric() {
			var gotResult, gotMode string
			for _, lp := range mm.GetLabel() {
				switch lp.GetName() {
				case "result":
					gotResult = lp.GetValue()
				case "resume_mode":
					gotMode = lp.GetValue()
				}
			}
			if gotResult == result && gotMode == mode {
				return mm.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// fakeProc simulates a claude process for recovery tests.
// Close deadCh to make Wait() return (simulating process death).
// Read blocks until deadCh is closed, then returns io.EOF.
type fakeProc struct {
	mu      sync.Mutex
	written []byte
	output  []byte // PTY bytes Read emits (once) before blocking until death

	deadCh  chan struct{} // close to kill
	waitErr error
}

func newFakeProc() *fakeProc {
	return &fakeProc{deadCh: make(chan struct{})}
}

// newFakeProcWithOutput seeds the PTY output the proc emits on boot (e.g. a
// "Bypass Permissions mode" dialog) so it lands in the manager's ring buffer.
func newFakeProcWithOutput(out string) *fakeProc {
	return &fakeProc{deadCh: make(chan struct{}), output: []byte(out)}
}

func (f *fakeProc) kill() {
	select {
	case <-f.deadCh:
	default:
		close(f.deadCh)
	}
}

func (f *fakeProc) Wait() error {
	<-f.deadCh
	return f.waitErr
}

func (f *fakeProc) Read(p []byte) (int, error) {
	f.mu.Lock()
	if len(f.output) > 0 {
		n := copy(p, f.output)
		f.output = f.output[n:]
		f.mu.Unlock()
		return n, nil
	}
	f.mu.Unlock()
	<-f.deadCh
	return 0, io.EOF
}

func (f *fakeProc) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written = append(f.written, p...)
	return len(p), nil
}

func (f *fakeProc) Close() error { return nil }

func (f *fakeProc) bytes() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.written...)
}

// spawnTracker records spawn calls made by the manager.
type spawnTracker struct {
	mu         sync.Mutex
	callCount  int
	lastResume bool
	procs      []*fakeProc // pre-allocated procs to return in order
	idx        int
	spawnErr   error
}

func newSpawnTracker(procs ...*fakeProc) *spawnTracker {
	return &spawnTracker{procs: procs}
}

func (st *spawnTracker) spawn(cfg session.Config, resume bool) (session.ClaudeProcess, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.callCount++
	st.lastResume = resume
	if st.spawnErr != nil {
		return nil, st.spawnErr
	}
	if st.idx >= len(st.procs) {
		// Return a proc that never dies (safety valve)
		return newFakeProc(), nil
	}
	p := st.procs[st.idx]
	st.idx++
	return p, nil
}

func (st *spawnTracker) calls() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.callCount
}

func (st *spawnTracker) resume() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.lastResume
}

// newRecoverMgr builds a Manager wired with a custom spawn function and a
// short BootTimeout so bootWait exits fast (BootTimeout < minBoot=4s).
func newRecoverMgr(t *testing.T, ids []string, maxRestarts int, st *spawnTracker) (*session.Manager, *turn.Store) {
	t.Helper()
	store := turn.NewStore()
	idx := 0
	m := session.New(
		session.Config{
			TurnTimeout: 10 * time.Second,
			BootTimeout: 30 * time.Millisecond, // < minBoot so bootWait exits fast
			SubmitDelay: 0,
			SubmitSeq:   session.DefaultSubmitSeq,
			MaxRestarts: maxRestarts,
		},
		store,
		metrics.New(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() time.Time { return time.Unix(100, 0) },
		func() string {
			if idx < len(ids) {
				s := ids[idx]
				idx++
				return s
			}
			return "unknown"
		},
	)
	m.SetSpawnForTest(st.spawn)
	return m, store
}

// injectAndStart injects the first fakeProc as the running process and starts
// the watch goroutine. The Manager state must already be Booting (default after
// New). We bypass Start() to avoid touching real PTYs.
func injectAndStart(t *testing.T, mgr *session.Manager, first *fakeProc) {
	t.Helper()
	mgr.InjectProcForTest(first)
}

// TestClaudeArgs_ContinueOnResume verifies --continue is added iff resume==true.
func TestClaudeArgs_ContinueOnResume(t *testing.T) {
	args := session.ConfigClaudeArgs(session.Config{}, true)
	require.Contains(t, args, "--continue", "resume=true must include --continue")

	args2 := session.ConfigClaudeArgs(session.Config{}, false)
	for _, a := range args2 {
		require.NotEqual(t, "--continue", a, "resume=false must NOT include --continue")
	}
}

func TestClaudeArgs_ResumeSessionIDOnInitialBoot(t *testing.T) {
	// Initial boot (resume=false) with a ResumeSessionID: cross-pod resume by id.
	args := session.ConfigClaudeArgs(session.Config{ResumeSessionID: "sid-123"}, false)
	idx := -1
	for i, a := range args {
		if a == "--resume" {
			idx = i
			break
		}
	}
	require.GreaterOrEqual(t, idx, 0, "expected --resume on initial boot with ResumeSessionID")
	require.Less(t, idx+1, len(args), "--resume missing its value")
	require.Equal(t, "sid-123", args[idx+1])
	require.NotContains(t, args, "--continue", "initial cross-pod resume uses --resume, not --continue")
}

func TestClaudeArgs_CrashRelaunchPrefersContinueOverResumeID(t *testing.T) {
	// A crash relaunch (resume=true) continues the most recent conversation even
	// when a ResumeSessionID is set (post-resume, that IS the most recent).
	args := session.ConfigClaudeArgs(session.Config{ResumeSessionID: "sid-123"}, true)
	require.Contains(t, args, "--continue")
	require.NotContains(t, args, "--resume", "crash relaunch must not also pass --resume")
}

func TestClaudeArgs_NoResumeWhenSessionIDEmpty(t *testing.T) {
	args := session.ConfigClaudeArgs(session.Config{}, false)
	require.NotContains(t, args, "--resume", "no ResumeSessionID must NOT emit --resume")
}

func TestClaudeArgs_EffortFlag(t *testing.T) {
	args := session.ConfigClaudeArgs(session.Config{Effort: "xhigh"}, false)
	// --effort and its value must be adjacent, in that order.
	idx := -1
	for i, a := range args {
		if a == "--effort" {
			idx = i
			break
		}
	}
	require.GreaterOrEqual(t, idx, 0, "expected --effort in args")
	require.Less(t, idx+1, len(args), "--effort missing its value")
	require.Equal(t, "xhigh", args[idx+1], "--effort value must be the level")
}

func TestClaudeArgs_NoEffortWhenEmpty(t *testing.T) {
	args := session.ConfigClaudeArgs(session.Config{}, false)
	for _, a := range args {
		require.NotEqual(t, "--effort", a, "empty Effort must NOT emit --effort")
	}
}

// TestWatch_MidTurnDeath_RelaunchesAndResumes: kill claude mid-turn; expect
// relaunch with resume=true and the turn re-submitted to the new proc.
func TestWatch_MidTurnDeath_RelaunchesAndResumes(t *testing.T) {
	first := newFakeProc()
	second := newFakeProc()
	// third proc never dies (safety valve for the new watch goroutine)
	third := newFakeProc()

	st := newSpawnTracker(second, third)

	mgr, store := newRecoverMgr(t, []string{"turn-1"}, 3, st)
	injectAndStart(t, mgr, first)

	// Submit a turn before killing
	id, err := mgr.Submit("hello world", "https://cb/x")
	require.NoError(t, err)
	require.Equal(t, "turn-1", id)

	// Verify turn is running
	rec, ok := store.Get("turn-1")
	require.True(t, ok)
	require.Equal(t, turn.Running, rec.State)

	// Kill the first proc (simulate crash)
	first.kill()

	// Wait for relaunch: spawn should be called for second proc with resume=true
	require.Eventually(t, func() bool { return st.calls() >= 1 }, 2*time.Second, 10*time.Millisecond,
		"spawn not called for relaunch")
	require.True(t, st.resume(), "relaunch must use resume=true (--continue)")

	// The second proc should receive the resume nudge (a plain submit keystroke).
	// resumeTurn does NOT re-paste the original prompt because --continue already
	// restored the full conversation (finding 1 fix: avoids double-submit).
	require.Eventually(t, func() bool {
		return len(second.bytes()) > 0
	}, 2*time.Second, 10*time.Millisecond, "second proc did not receive resume nudge")

	w := string(second.bytes())
	assert.Contains(t, w, session.DefaultSubmitSeq.Submit, "resume nudge (submit keystroke) must be sent to new proc")
	// The full prompt must NOT be re-pasted (would cause double-submit via --continue).
	assert.NotContains(t, w, "hello world", "full prompt must not be re-pasted on --continue resume")

	// Turn must still be the same id, not failed
	rec2, ok2 := store.Get("turn-1")
	require.True(t, ok2)
	require.Equal(t, turn.Running, rec2.State, "turn must still be running after relaunch")
}

// TestWatch_DeathAtCap_FailsFastAndStaysDead: exhaust the restart budget.
func TestWatch_DeathAtCap_FailsFastAndStaysDead(t *testing.T) {
	first := newFakeProc()
	second := newFakeProc()

	st := newSpawnTracker(second)

	done := make(chan *turn.Record, 2)

	mgr, store := newRecoverMgr(t, []string{"turn-1"}, 1, st)
	mgr.OnTurnDone = func(r *turn.Record) { done <- r }
	injectAndStart(t, mgr, first)

	id, err := mgr.Submit("work", "https://cb/")
	require.NoError(t, err)
	require.Equal(t, "turn-1", id)

	// First death -> relaunch #1 (within budget)
	first.kill()
	require.Eventually(t, func() bool { return st.calls() >= 1 }, 2*time.Second, 10*time.Millisecond,
		"first relaunch not triggered")

	// Wait for second proc to be ready and receive re-submitted turn
	require.Eventually(t, func() bool {
		return len(second.bytes()) > 0
	}, 2*time.Second, 10*time.Millisecond, "second proc did not get re-submitted turn")

	// Second death -> over cap (MaxRestarts=1, attempt=2 > 1)
	second.kill()

	// Turn must be failed and OnTurnDone fired
	select {
	case r := <-done:
		require.Equal(t, turn.Failed, r.State)
		require.Contains(t, r.Error, "restart budget")
	case <-time.After(3 * time.Second):
		t.Fatal("OnTurnDone not fired after budget exhausted")
	}

	// State stays Dead, spawn NOT called a 3rd time
	rec, _ := store.Get("turn-1")
	require.Equal(t, turn.Failed, rec.State)
	snap := mgr.Snapshot()
	require.Equal(t, session.Dead, snap.State)
	require.Equal(t, 1, st.calls(), "spawn must not be called beyond budget")
}

// TestWatch_IdleDeath_Relaunches: no in-flight turn when crash happens;
// session should relaunch and become Ready again.
func TestWatch_IdleDeath_Relaunches(t *testing.T) {
	first := newFakeProc()
	second := newFakeProc()
	third := newFakeProc()

	st := newSpawnTracker(second, third)

	mgr, _ := newRecoverMgr(t, []string{"turn-1"}, 3, st)
	injectAndStart(t, mgr, first)

	// No turn submitted; kill while idle
	first.kill()

	// Should relaunch
	require.Eventually(t, func() bool { return st.calls() >= 1 }, 2*time.Second, 10*time.Millisecond,
		"spawn not called after idle death")

	// State should eventually become Ready (bootWait will exit fast due to short BootTimeout)
	require.Eventually(t, func() bool {
		snap := mgr.Snapshot()
		return snap.State == session.Ready
	}, 2*time.Second, 10*time.Millisecond, "session did not return to Ready after idle relaunch")
}

// TestComplete_ResetsRestartCounter: kill once (restarts=1), relaunch, then
// Complete a turn; next kill should relaunch again (counter reset).
func TestComplete_ResetsRestartCounter(t *testing.T) {
	first := newFakeProc()
	second := newFakeProc()
	third := newFakeProc()
	fourth := newFakeProc()

	st := newSpawnTracker(second, third, fourth)

	mgr, _ := newRecoverMgr(t, []string{"turn-1", "turn-2"}, 1, st)
	injectAndStart(t, mgr, first)

	// Submit turn-1
	_, err := mgr.Submit("first turn", "")
	require.NoError(t, err)

	// Kill -> relaunch #1 (restarts=1, at cap for MaxRestarts=1)
	first.kill()
	require.Eventually(t, func() bool { return st.calls() >= 1 }, 2*time.Second, 10*time.Millisecond)

	// Wait for second proc to receive re-submitted turn
	require.Eventually(t, func() bool { return len(second.bytes()) > 0 }, 2*time.Second, 10*time.Millisecond)

	// Complete the turn -> restarts reset to 0
	err = mgr.Complete(session.HookResult{FinalText: "done", StopReason: "end_turn"})
	require.NoError(t, err)

	// Now submit turn-2 and kill second proc -> should relaunch (restarts=1 again, not 2)
	_, err = mgr.Submit("second turn", "")
	require.NoError(t, err)

	second.kill()
	require.Eventually(t, func() bool { return st.calls() >= 2 }, 2*time.Second, 10*time.Millisecond,
		"second relaunch not triggered (restart counter not reset by Complete)")
}

// TestRelaunch_ResetsRingNoStaleBypassAccept: the dead proc's "Bypass
// Permissions mode" dialog text must not leak into the relaunched proc's boot
// navigation. Before the ring reset, bootWait matched the stale string and fired
// the accept keystrokes (Down+Enter) into the still-initializing TUI. With the
// reset, the relaunched proc (which has not yet drawn its own dialog) sees no
// match and bootWait stays its hand.
func TestRelaunch_ResetsRingNoStaleBypassAccept(t *testing.T) {
	// first boots and prints the bypass dialog; second (post-relaunch) withholds it.
	first := newFakeProcWithOutput("\x1b[1mWARNING:\x1b[2GBypass\x1b[9GPermissions\x1b[21Gmode\x1b[0m")
	second := newFakeProc()
	third := newFakeProc() // safety valve for the new watch goroutine

	st := newSpawnTracker(second, third)

	mgr, _ := newRecoverMgr(t, []string{"turn-1"}, 3, st)
	injectAndStart(t, mgr, first)

	// The stale dialog must actually be resident in the ring before the crash,
	// otherwise the reset would be a no-op and the test would pass vacuously.
	require.Eventually(t, func() bool {
		return mgr.RingContainsForTest("Bypass Permissions mode")
	}, 2*time.Second, 10*time.Millisecond, "first proc's dialog never reached the ring")

	// Crash -> relaunch (resets the ring) -> bootWait(second).
	first.kill()
	require.Eventually(t, func() bool {
		return st.calls() >= 1 && mgr.Snapshot().State == session.Ready
	}, 2*time.Second, 10*time.Millisecond, "relaunch did not return the session to Ready")

	// The reset cleared the stale dialog...
	require.False(t, mgr.RingContainsForTest("Bypass Permissions mode"),
		"relaunch must reset the ring so stale dialog text does not persist")
	// ...so bootWait never sent the accept keystroke to the relaunched proc.
	assert.NotContains(t, string(second.bytes()), "\x1b[B",
		"bootWait must not fire bypass-accept against a stale dialog after relaunch")
}

// TestWatch_ShutdownDeath_NoRelaunch: stopping=true when watch fires; must
// NOT relaunch and state stays Dead.
func TestWatch_ShutdownDeath_NoRelaunch(t *testing.T) {
	first := newFakeProc()
	// no further procs needed
	st := newSpawnTracker()

	var spawnCalled int32
	origSpawn := st.spawn
	st2 := &spawnTracker{procs: st.procs}
	st2.spawnErr = nil
	_ = origSpawn // use the real tracker

	mgr, _ := newRecoverMgr(t, []string{"turn-1"}, 3, st)
	injectAndStart(t, mgr, first)

	// Shutdown sets stopping=true, marks Dead, closes PTY
	// We don't call Shutdown (which would close the proc) - instead we set
	// stopping directly via the test hook then kill.
	mgr.SetStoppingForTest()

	// Kill after stopping is set -> watch sees stopping=true
	first.kill()

	// spawn must not be called
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, 0, st.calls(), "spawn must not be called when stopping=true")
	_ = spawnCalled

	snap := mgr.Snapshot()
	require.Equal(t, session.Dead, snap.State)
}

// newResumeMgr builds a Manager with explicit registry/store wired so the test
// can assert metrics and turn state after a crash+relaunch resume.
func newResumeMgr(t *testing.T, reg *prometheus.Registry, st *spawnTracker) (*session.Manager, *turn.Store) {
	t.Helper()
	store := turn.NewStore()
	m := session.New(
		session.Config{
			TurnTimeout: 10 * time.Second,
			BootTimeout: 30 * time.Millisecond,
			SubmitDelay: 0,
			SubmitSeq:   session.DefaultSubmitSeq,
			MaxRestarts: 3,
		},
		store,
		metrics.New(reg),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() time.Time { return time.Unix(100, 0) },
		func() string { return "turn-1" },
	)
	m.SetSpawnForTest(st.spawn)
	return m, store
}

// TestResumeTurn_CompletesFromTranscript_NoNudge: claude finished the turn before
// the crash (transcript ends with a terminal assistant answer). resumeTurn must
// NOT send the bare-CR nudge (which would inject an empty user turn and trigger
// duplicate work); instead it resolves the turn from the transcript.
func TestResumeTurn_CompletesFromTranscript_NoNudge(t *testing.T) {
	dir := t.TempDir()
	tp := filepath.Join(dir, "session.jsonl")
	lines := `{"type":"user","message":{"role":"user","content":"do the thing"}}
{"type":"assistant","message":{"model":"claude-opus-4-8","content":[{"type":"text","text":"all done"}],"usage":{"input_tokens":10,"output_tokens":3},"stop_reason":"end_turn"}}
`
	require.NoError(t, os.WriteFile(tp, []byte(lines), 0o644))

	reg := prometheus.NewRegistry()
	first := newFakeProc()
	second := newFakeProc()
	third := newFakeProc()
	st := newSpawnTracker(second, third)

	mgr, store := newResumeMgr(t, reg, st)
	done := make(chan *turn.Record, 1)
	mgr.OnTurnDone = func(r *turn.Record) { done <- r }
	mgr.InjectProcForTest(first)

	_, err := mgr.Submit("do the thing", "")
	require.NoError(t, err)
	// A prior turn's hook would have set this in production; the session JSONL
	// accumulates across turns and now contains this turn's completed answer.
	mgr.SetTranscriptPathForTest(tp)

	first.kill() // crash -> watch -> relaunch -> resumeTurn

	select {
	case r := <-done:
		require.Equal(t, turn.Complete, r.State, "turn must be completed from transcript")
		require.Equal(t, "all done", r.FinalText)
		require.Equal(t, "end_turn", r.StopReason)
	case <-time.After(3 * time.Second):
		t.Fatal("OnTurnDone not fired; turn never completed from transcript")
	}

	// The relaunched proc must NOT receive a submit keystroke: no empty nudge.
	assert.Empty(t, second.bytes(), "completed-from-transcript resume must not nudge the relaunched proc")

	rec, ok := store.Get("turn-1")
	require.True(t, ok)
	require.Equal(t, turn.Complete, rec.State)

	require.Equal(t, float64(1), resumeModeCount(t, reg, "ok", "complete_from_transcript"),
		"TurnResumes{ok,complete_from_transcript} must be 1")
	require.Equal(t, float64(0), resumeModeCount(t, reg, "ok", "nudge"),
		"no nudge resume should be recorded")
}

// TestResumeTurn_NudgesWhenTranscriptPending: the transcript ends with the
// restored user prompt (turn genuinely in flight). resumeTurn must fall back to
// today's behavior: send the bare-CR nudge and keep the turn running.
func TestResumeTurn_NudgesWhenTranscriptPending(t *testing.T) {
	dir := t.TempDir()
	tp := filepath.Join(dir, "session.jsonl")
	require.NoError(t, os.WriteFile(tp, []byte(
		`{"type":"user","message":{"role":"user","content":"keep going"}}`+"\n"), 0o644))

	reg := prometheus.NewRegistry()
	first := newFakeProc()
	second := newFakeProc()
	third := newFakeProc()
	st := newSpawnTracker(second, third)

	mgr, store := newResumeMgr(t, reg, st)
	mgr.InjectProcForTest(first)

	_, err := mgr.Submit("keep going", "")
	require.NoError(t, err)
	mgr.SetTranscriptPathForTest(tp)

	first.kill()

	require.Eventually(t, func() bool { return len(second.bytes()) > 0 },
		3*time.Second, 10*time.Millisecond, "pending-transcript resume must nudge the relaunched proc")
	assert.Contains(t, string(second.bytes()), session.DefaultSubmitSeq.Submit)

	rec, ok := store.Get("turn-1")
	require.True(t, ok)
	require.Equal(t, turn.Running, rec.State, "pending turn must stay running after nudge")

	require.Equal(t, float64(1), resumeModeCount(t, reg, "ok", "nudge"),
		"TurnResumes{ok,nudge} must be 1")
	require.Equal(t, float64(0), resumeModeCount(t, reg, "ok", "complete_from_transcript"),
		"no completion resume should be recorded")
}
