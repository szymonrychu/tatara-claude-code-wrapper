package session_test

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

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

	_, err := mgr.Submit("hello", "")
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

	_, err := mgr.Submit("hello", "")
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

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriter) Close() error                { return nil }
