package session_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestOnRestart_FiresOnResumeRelaunch asserts the conversationRestart callback
// runs after a crash-relaunch that resumed an existing conversation (a
// transcript was persisted, so shouldResume() is true and spawn uses
// --continue).
func TestOnRestart_FiresOnResumeRelaunch(t *testing.T) {
	first := newFakeProc()
	second := newFakeProc()
	third := newFakeProc()
	st := newSpawnTracker(second, third)

	mgr, _ := newRecoverMgr(t, []string{"turn-1"}, 3, st)
	restarted := make(chan struct{}, 1)
	mgr.OnRestart = func() { restarted <- struct{}{} }
	injectAndStart(t, mgr, first)

	// Submit a turn so the conversation exists -> relaunch resumes (--continue).
	_, err := mgr.Submit("hello", "https://cb/x")
	require.NoError(t, err)
	// A prior turn's hook would have persisted a transcript in production; an
	// in-flight turn alone (mgr.current set) is not enough to resume.
	mgr.SetTranscriptPathForTest(filepath.Join(t.TempDir(), "session.jsonl"))

	first.kill()

	require.Eventually(t, func() bool { return st.calls() >= 1 }, 2*time.Second, 10*time.Millisecond)
	require.True(t, st.resume(), "relaunch must resume")

	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("OnRestart was not fired after a resume relaunch")
	}
}

// TestOnRestart_NotFiredOnFreshFirstBootRelaunch asserts the callback does NOT
// run when the relaunch is a fresh restart (no conversation to resume: idle
// death before any turn).
func TestOnRestart_NotFiredOnFreshFirstBootRelaunch(t *testing.T) {
	first := newFakeProc()
	second := newFakeProc()
	third := newFakeProc()
	st := newSpawnTracker(second, third)

	mgr, _ := newRecoverMgr(t, []string{"turn-1"}, 3, st)
	fired := make(chan struct{}, 1)
	mgr.OnRestart = func() { fired <- struct{}{} }
	injectAndStart(t, mgr, first)

	// No turn submitted: idle death -> fresh relaunch (resume=false).
	first.kill()

	require.Eventually(t, func() bool { return st.calls() >= 1 }, 2*time.Second, 10*time.Millisecond)
	require.False(t, st.resume(), "idle-death relaunch must be fresh (resume=false)")

	select {
	case <-fired:
		t.Fatal("OnRestart must not fire on a fresh first-boot relaunch")
	case <-time.After(300 * time.Millisecond):
		// good: not fired
	}
}
