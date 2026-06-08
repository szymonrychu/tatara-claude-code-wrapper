package bootstrap_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
)

func TestRender_ExcludesWrapperConfigFromGit(t *testing.T) {
	ws := t.TempDir()
	p := bootstrap.Params{
		HomeDir: t.TempDir(), Workspace: ws,
		BaseMCP:     []byte(`{"mcpServers":{}}`),
		RepoURL:     "https://github.com/x/y",
		RepoBranch:  "main",
		HookCommand: "/usr/local/bin/cc-stop-hook", PermissionMode: "bypassPermissions",
	}
	require.NoError(t, bootstrap.Render(p, func(a ...string) error { return nil }))

	// Wrapper-injected session files must be excluded so the agent's commit
	// carries only its real edits, not .mcp.json / .claude/ scaffolding.
	b, err := os.ReadFile(filepath.Join(ws, ".git", "info", "exclude"))
	require.NoError(t, err, "git exclude not written")
	got := string(b)
	require.Contains(t, got, ".mcp.json")
	require.Contains(t, got, ".claude/")
}

func TestRender_ChecksOutTaskBranchAfterClone(t *testing.T) {
	var calls [][]string
	p := bootstrap.Params{
		HomeDir: t.TempDir(), Workspace: t.TempDir(),
		BaseMCP:     []byte(`{"mcpServers":{}}`),
		RepoURL:     "https://github.com/x/y",
		RepoBranch:  "main",
		TaskBranch:  "tatara/task-abc",
		HookCommand: "/usr/local/bin/cc-stop-hook", PermissionMode: "bypassPermissions",
	}
	require.NoError(t, bootstrap.Render(p, func(a ...string) error { calls = append(calls, a); return nil }))

	cloneIdx, coIdx := -1, -1
	for i, c := range calls {
		j := strings.Join(c, " ")
		if strings.Contains(j, "clone") {
			cloneIdx = i
		}
		if strings.Contains(j, "checkout") && strings.Contains(j, "tatara/task-abc") {
			coIdx = i
		}
	}
	require.GreaterOrEqual(t, cloneIdx, 0, "repo not cloned")
	require.GreaterOrEqual(t, coIdx, 0, "task branch not checked out")
	require.Less(t, cloneIdx, coIdx, "checkout must run after clone")
}

func TestCommitAndPush_CommitsWhenDirtyThenPushes(t *testing.T) {
	var calls [][]string
	git := func(a ...string) error {
		calls = append(calls, a)
		// `diff --cached --quiet` exits non-zero when there are staged changes.
		if len(a) >= 3 && a[0] == "diff" && a[1] == "--cached" && a[2] == "--quiet" {
			return errors.New("exit status 1")
		}
		return nil
	}
	require.NoError(t, bootstrap.CommitAndPush("tatara/task-abc", "agent work", git))

	var all []string
	for _, c := range calls {
		all = append(all, strings.Join(c, " "))
	}
	joined := strings.Join(all, "|")
	require.Contains(t, joined, "add -A")
	require.Contains(t, joined, "commit -m agent work")
	require.Contains(t, joined, "push -u origin tatara/task-abc")
}

func TestCommitAndPush_SkipsCommitWhenClean(t *testing.T) {
	var committed, pushed bool
	git := func(a ...string) error {
		if len(a) >= 1 && a[0] == "commit" {
			committed = true
		}
		if len(a) >= 1 && a[0] == "push" {
			pushed = true
		}
		return nil // diff --cached --quiet returns nil -> nothing staged
	}
	require.NoError(t, bootstrap.CommitAndPush("b", "m", git))
	require.False(t, committed, "must not commit when nothing is staged")
	require.True(t, pushed, "branch must still be pushed so write-back can open the PR")
}
