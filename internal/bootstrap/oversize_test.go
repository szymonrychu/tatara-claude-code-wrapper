package bootstrap_test

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
)

// dirtyTreeGit records every git call and reports a dirty index, so
// CommitAndPush proceeds past its `diff --cached --quiet` clean-tree shortcut.
func dirtyTreeGit(calls *[][]string) bootstrap.GitRunner {
	return func(_ string, a ...string) error {
		*calls = append(*calls, a)
		if len(a) > 1 && a[0] == "diff" {
			return errDirtyIndex
		}
		return nil
	}
}

var errDirtyIndex = errors.New("exit status 1")

// TestCommitAndPush_UnstagesOversizedBlob covers the other half of issue #131:
// `git add -A` plus `git commit --no-verify` swept a 15.7 MB build artifact
// onto the task branch, bypassing the pre-commit size hook, and every later
// turn inherited it. An oversized file must be unstaged (loudly), while the
// rest of the agent's work still commits and pushes.
func TestCommitAndPush_UnstagesOversizedBlob(t *testing.T) {
	orig := bootstrap.MaxStagedBlobBytes
	bootstrap.MaxStagedBlobBytes = 1024
	t.Cleanup(func() { bootstrap.MaxStagedBlobBytes = orig })

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "wrapper"), make([]byte, 4096), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "objects", "pack"), make([]byte, 4096), 0o644))

	var calls [][]string
	git := dirtyTreeGit(&calls)

	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)

	pushed, err := bootstrap.CommitAndPush(dir, "tatara/task-abc", "agent work", git, log, m)
	require.NoError(t, err)
	require.True(t, pushed, "the rest of the turn's work must still commit and push")

	require.NotEmpty(t, callsEqual(calls, "reset", "-q", "--", "wrapper"),
		"the oversized build artifact must be unstaged before the commit")
	require.Empty(t, callsEqual(calls, "reset", "-q", "--", "main.go"),
		"normal source files must not be unstaged")
	require.Empty(t, callsContainingAll(calls, "reset", "-q", "--", filepath.Join(".git", "objects", "pack")),
		"git's own object store must never be walked")

	require.Equal(t, float64(1), counterValue(t, reg, "ccw_commit_oversized_blob_skipped_total", ""))
	require.Contains(t, logBuf.String(), `"level":"WARN"`)
	require.Contains(t, logBuf.String(), "wrapper")
	require.Contains(t, logBuf.String(), "commit_oversized_blob")
}

// TestCommitAndPush_UnstageRunsBeforeCommit asserts ordering: unstaging after
// the commit would be useless.
func TestCommitAndPush_UnstageRunsBeforeCommit(t *testing.T) {
	orig := bootstrap.MaxStagedBlobBytes
	bootstrap.MaxStagedBlobBytes = 1024
	t.Cleanup(func() { bootstrap.MaxStagedBlobBytes = orig })

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "wrapper"), make([]byte, 4096), 0o755))

	var calls [][]string
	git := dirtyTreeGit(&calls)
	_, err := bootstrap.CommitAndPush(dir, "b", "m", git, nil, nil)
	require.NoError(t, err)

	resetAt, commitAt := -1, -1
	for i, c := range calls {
		if c[0] == "reset" && resetAt == -1 {
			resetAt = i
		}
		if c[0] == "commit" {
			commitAt = i
		}
	}
	require.NotEqual(t, -1, resetAt)
	require.NotEqual(t, -1, commitAt)
	require.Less(t, resetAt, commitAt, "oversized blobs must be unstaged before the commit")
}

// TestCommitAndPush_NoOversizedFiles_NoReset asserts the guard is inert on a
// normal turn.
func TestCommitAndPush_NoOversizedFiles_NoReset(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644))

	var calls [][]string
	git := dirtyTreeGit(&calls)
	_, err := bootstrap.CommitAndPush(dir, "b", "m", git, nil, nil)
	require.NoError(t, err)
	require.Empty(t, callsContainingAll(calls, "reset"))
}
