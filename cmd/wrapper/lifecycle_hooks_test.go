package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
)

func testHookDeps(t *testing.T) (config, *metrics.Metrics, *slog.Logger) {
	t.Helper()
	cfg := config{Workspace: t.TempDir()}
	m := metrics.New(prometheus.NewRegistry())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return cfg, m, log
}

func TestFireLifecycleHook_RunsCommandWithEnv(t *testing.T) {
	cfg, m, log := testHookDeps(t)
	out := filepath.Join(cfg.Workspace, "out.txt")
	fireLifecycleHook(cfg, m, log, "agentTurnFinished",
		`printf '%s' "$TATARA_TURN_ID" > `+out, []string{"TATARA_TURN_ID=turn-9"})

	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, "turn-9", string(b))
}

func TestFireLifecycleHook_EmptyCommandNoop(t *testing.T) {
	cfg, m, log := testHookDeps(t)
	// Empty command must not run anything and must not panic.
	fireLifecycleHook(cfg, m, log, "conversationStart", "", nil)
	entries, err := os.ReadDir(cfg.Workspace)
	require.NoError(t, err)
	require.Empty(t, entries, "empty command must not create files")
}

func TestFireLifecycleHookBounded_ReturnsBeforeSlowHookFinishes(t *testing.T) {
	cfg, m, log := testHookDeps(t)
	start := time.Now()
	// The hook sleeps far longer than the bound; the call must return promptly.
	fireLifecycleHookBounded(context.Background(), cfg, m, log, "conversationFinished", "sleep 5", 50*time.Millisecond)
	require.Less(t, time.Since(start), 2*time.Second, "bounded hook must not block for the full sleep")
}

func TestFireLifecycleHookBounded_RunsFastCommand(t *testing.T) {
	cfg, m, log := testHookDeps(t)
	out := filepath.Join(cfg.Workspace, "done.txt")
	fireLifecycleHookBounded(context.Background(), cfg, m, log, "conversationFinished",
		"printf done > "+out, 2*time.Second)
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, "done", string(b))
}

func TestFireLifecycleHookBounded_EmptyCommandNoop(t *testing.T) {
	cfg, m, log := testHookDeps(t)
	// Empty command returns immediately without spawning a goroutine.
	start := time.Now()
	fireLifecycleHookBounded(context.Background(), cfg, m, log, "conversationFinished", "", time.Hour)
	require.Less(t, time.Since(start), time.Second)
}
