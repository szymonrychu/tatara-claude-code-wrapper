package session_test

import (
	"io"
	"log/slog"
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

// fakeProc simulates a claude process for recovery tests.
// Close deadCh to make Wait() return (simulating process death).
// Read blocks until deadCh is closed, then returns io.EOF.
type fakeProc struct {
	mu      sync.Mutex
	written []byte

	deadCh  chan struct{} // close to kill
	waitErr error
}

func newFakeProc() *fakeProc {
	return &fakeProc{deadCh: make(chan struct{})}
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

	// The second proc should receive the re-submitted turn text
	require.Eventually(t, func() bool {
		return len(second.bytes()) > 0
	}, 2*time.Second, 10*time.Millisecond, "second proc did not receive re-submitted turn")

	w := string(second.bytes())
	assert.Contains(t, w, "hello world", "turn text must be re-submitted to new proc")

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
