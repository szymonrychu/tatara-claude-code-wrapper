package session_test

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/convstore"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

// newBareMgrWithConfig is like newBareMgr but lets the caller set HomeDir/
// Workspace (needed for the on-disk transcript check), while keeping the same
// timing/seq defaults.
func newBareMgrWithConfig(t *testing.T, cfg session.Config) *session.Manager {
	t.Helper()
	cfg.TurnTimeout = 10 * time.Second
	cfg.BootTimeout = 30 * time.Millisecond
	cfg.SubmitSeq = session.DefaultSubmitSeq
	cfg.MaxRestarts = 3
	store := turn.NewStore()
	return session.New(
		cfg,
		store,
		metrics.New(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() time.Time { return time.Unix(100, 0) },
		func() string { return "turn-1" },
	)
}

// newBareMgr builds a Manager with no spawn wiring; only shouldResume-relevant
// state is exercised (no watch/relaunch goroutines are started).
func newBareMgr(t *testing.T) *session.Manager {
	t.Helper()
	store := turn.NewStore()
	return session.New(
		session.Config{
			TurnTimeout: 10 * time.Second,
			BootTimeout: 30 * time.Millisecond,
			SubmitSeq:   session.DefaultSubmitSeq,
			MaxRestarts: 3,
		},
		store,
		metrics.New(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() time.Time { return time.Unix(100, 0) },
		func() string { return "turn-1" },
	)
}

// TestShouldResume_InFlightTurnAloneIsNotEnough: an in-flight turn (mgr.current
// set) with no completed turn and no persisted transcript must NOT resume. This
// is the exact boot-crash shape: Submit() accepts a turn and sets mgr.current
// before claude has written any transcript; a crash in that window must relaunch
// fresh, never --continue (a fresh conversation has nothing to continue into).
func TestShouldResume_InFlightTurnAloneIsNotEnough(t *testing.T) {
	mgr := newBareMgr(t)
	mgr.SetWriterForTest(nopWriter{})

	_, err := mgr.Submit("hello", "", false)
	require.NoError(t, err)

	require.False(t, mgr.ShouldResumeForTest(),
		"in-flight turn with no completed turn and no persisted transcript must not resume")
}

// TestShouldResume_TurnsCompletedIsEnough: once a turn has completed at least
// once, a subsequent crash resumes even if the current transcript path field is
// unset (defense in depth on the invariant, not the specific field).
func TestShouldResume_TurnsCompletedIsEnough(t *testing.T) {
	mgr := newBareMgr(t)
	mgr.SetWriterForTest(nopWriter{})

	_, err := mgr.Submit("hello", "", false)
	require.NoError(t, err)
	require.NoError(t, mgr.Complete(session.HookResult{FinalText: "done", StopReason: "end_turn"}))

	require.True(t, mgr.ShouldResumeForTest(), "a completed turn proves a real conversation exists")
}

// TestShouldResume_TranscriptPathIsEnough: a persisted transcript path alone
// (set by a prior turn's Stop hook) is sufficient to resume, independent of
// turnsCompleted or an in-flight turn.
func TestShouldResume_TranscriptPathIsEnough(t *testing.T) {
	mgr := newBareMgr(t)
	mgr.SetTranscriptPathForTest("/tmp/fake/session.jsonl")

	require.True(t, mgr.ShouldResumeForTest(), "a persisted transcript path proves a real conversation exists")
}

// TestShouldResume_OnDiskTranscriptIsEnough: a genuine mid-first-turn crash can
// leave claude's own transcript JSONL on disk
// (~/.claude/projects/<dir>/<sid>.jsonl) even though the Stop hook never fired
// (so transcriptPath is still "") and no turn has completed. --continue's real
// precondition is "does a transcript exist on disk"; shouldResume must match
// that instead of discarding a genuinely resumable conversation just because
// the wrapper's own bookkeeping never observed it.
func TestShouldResume_OnDiskTranscriptIsEnough(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	dir := convstore.TranscriptDir(home, workspace)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "abc-123.jsonl"), []byte("{}"), 0o644))

	mgr := newBareMgrWithConfig(t, session.Config{HomeDir: home, Workspace: workspace})

	require.True(t, mgr.ShouldResumeForTest(),
		"an on-disk transcript proves --continue has something to resume into, even with turnsCompleted==0 and transcriptPath==''")
}

// TestShouldResume_NoOnDiskTranscript_StillFreshBootCrashSafe: the boot-crash
// invariant from the prior fix must still hold once the on-disk check is added:
// no transcript anywhere (neither a persisted path nor a jsonl on disk) must
// still not resume, even with HomeDir/Workspace set (an empty/absent transcript
// dir must glob to nothing).
func TestShouldResume_NoOnDiskTranscript_StillFreshBootCrashSafe(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	// Deliberately no jsonl written: TranscriptDir(home, workspace) is empty.

	mgr := newBareMgrWithConfig(t, session.Config{HomeDir: home, Workspace: workspace})
	mgr.SetWriterForTest(nopWriter{})

	_, err := mgr.Submit("hello", "", false)
	require.NoError(t, err)

	require.False(t, mgr.ShouldResumeForTest(),
		"in-flight turn with no completed turn, no persisted transcript, and no on-disk jsonl must not resume")
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriter) Close() error                { return nil }
