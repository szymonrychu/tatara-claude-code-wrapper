package session_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/convstore"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

// subagentLine builds an assistant line as it appears in a subagent's own
// transcript file: isSidechain, with a parentUuid back into the main one.
func subagentLine(uuid, text string) string {
	return `{"parentUuid":"uuid-parent-1","isSidechain":true,"type":"assistant","uuid":"` + uuid +
		`","sessionId":"sess-sub","timestamp":"2026-08-09T17:00:00.000Z","message":{"role":"assistant","content":[{"type":"text","text":"` +
		text + `"}]}}`
}

// TestSubagents_FollowedAndStampActivity is the observability half of W1.
//
// Claude Code writes a subagent's entire conversation to
// <sessionDir>/subagents/agent-<id>.jsonl. The wrapper's transcript discovery
// globs `<dir>/*.jsonl`, which is not recursive, so none of it was ever read:
// during one real subagent run the main transcript was silent for 2095 seconds
// (35 minutes) while the pod was working flat out.
//
// The file is created AFTER the turn starts, exactly like the main transcript,
// so this test writes it only once Submit has returned.
func TestSubagents_FollowedAndStampActivity(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	transcriptDir := convstore.TranscriptDir(home, workspace)
	require.NoError(t, os.MkdirAll(transcriptDir, 0o755))

	store := turn.NewStore()
	var nowCalls int64
	m := session.New(
		session.Config{
			TurnTimeout: 30 * time.Second,
			SubmitSeq:   session.DefaultSubmitSeq,
			HomeDir:     home,
			Workspace:   workspace,
		},
		store,
		metrics.New(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() time.Time { n := atomic.AddInt64(&nowCalls, 1); return time.Unix(100+n, 0) },
		func() string { return "turn-1" },
	)
	m.SetWriterForTest(&fakePTY{})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	m.StartTailer(ctx)

	require.True(t, m.Snapshot().LastSubagentActivityAt.IsZero(),
		"no subagent line read yet, so the stamp must be zero")

	_, err := m.Submit("go", "", false)
	require.NoError(t, err)

	subDir := filepath.Join(transcriptDir, "subagents")
	require.NoError(t, os.MkdirAll(subDir, 0o755))
	agentPath := filepath.Join(subDir, "agent-abc123.jsonl")
	require.NoError(t, os.WriteFile(agentPath, []byte(subagentLine("uuid-sub-1", "subagent working")+"\n"), 0o644))

	first := waitForSubagentStamp(t, m, time.Time{}, 10*time.Second)

	// A second line must advance the stamp: the point is a live heartbeat, not
	// a one-shot "a subagent existed at some point" flag.
	f, err := os.OpenFile(agentPath, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(subagentLine("uuid-sub-2", "still working") + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	waitForSubagentStamp(t, m, first, 10*time.Second)

	// The parent transcript's outstanding Task tool_use is the other half of
	// the signal: it says a subagent is running right now, which is what makes
	// parent-transcript silence interpretable instead of ambiguous.
	taskUse := `{"type":"assistant","uuid":"uuid-parent-1","sessionId":"sess-main","timestamp":"2026-08-09T17:00:00.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_task_1","name":"Task","input":{}}]}}`
	require.NoError(t, os.WriteFile(filepath.Join(transcriptDir, "sess-main.jsonl"), []byte(taskUse+"\n"), 0o644))

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m.Snapshot().OutstandingSubagentCalls == 1 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("outstandingSubagentCalls = %d, want 1: a running subagent is invisible",
		m.Snapshot().OutstandingSubagentCalls)
}

// waitForSubagentStamp polls Snapshot until LastSubagentActivityAt is strictly
// after after, returning the observed value.
func waitForSubagentStamp(t *testing.T, m *session.Manager, after time.Time, timeout time.Duration) time.Time {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		got := m.Snapshot().LastSubagentActivityAt
		if got.After(after) {
			return got
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("lastSubagentActivityAt never advanced past %s: subagent transcripts are still invisible to the wrapper", after)
	return time.Time{}
}

// TestSubagentActivity_DoesNotResetTurnTimer is the regression that keeps W1 a
// pure-observation change.
//
// Subagent lines were previously never read at all, so feeding them into the
// turn's inactivity timer would make turns that used to time out stop timing
// out - a behaviour change in a phase whose entire contract is that nothing
// starts failing, killing or timing out differently. The stamp is published
// through Snapshot; deciding what to do with it belongs to the operator.
//
// Mirrors TestActivity_StaleTurnDoesNotResetTimer: hammer the hook across a
// window longer than TurnTimeout and require the turn to have timed out anyway.
func TestSubagentActivity_DoesNotResetTurnTimer(t *testing.T) {
	fp := &fakePTY{}
	m, _ := newMgr(t, fp) // TurnTimeout 50ms
	done := make(chan *turn.Record, 1)
	m.OnTurnDone = func(r *turn.Record) { done <- r }

	id, err := m.Submit("hi", "https://cb/x", false)
	require.NoError(t, err)

	for i := 0; i < 8; i++ {
		m.FireSubagentActivityForTest(id)
		time.Sleep(20 * time.Millisecond)
	}

	select {
	case r := <-done:
		require.Equal(t, turn.Failed, r.State)
	default:
		t.Fatal("subagent activity reset the turn's inactivity timer: W1 is supposed to be observation only, and this silently changes when turns time out")
	}

	// The observation itself must still have been recorded.
	require.False(t, m.Snapshot().LastSubagentActivityAt.IsZero(),
		"the hook must stamp the timestamp even though it does not touch the timer")
}

// TestSnapshot_StallFieldsPresentAndZero pins the additive-only shape of the
// new Snapshot fields. ProbePendingSince and StallSuspectedSince exist so the
// operator side has a stable JSON shape from this release on, but nothing sets
// them until later phases; a non-zero value here would mean behaviour landed
// early.
func TestSnapshot_StallFieldsPresentAndZero(t *testing.T) {
	fp := &fakePTY{}
	m, _ := newMgr(t, fp)

	snap := m.Snapshot()
	require.True(t, snap.ProbePendingSince.IsZero(), "no probe exists in this phase")
	require.True(t, snap.StallSuspectedSince.IsZero(), "nothing marks a stall in this phase")
	require.True(t, snap.LastSubagentActivityAt.IsZero())
	require.Equal(t, int64(0), snap.OutstandingSubagentCalls)
}
