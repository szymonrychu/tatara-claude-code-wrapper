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

func TestRender_ClonesEachRepoIntoSubdirAndChecksOutBranch(t *testing.T) {
	ws := t.TempDir()
	var calls [][]string // dir + args
	p := bootstrap.Params{
		HomeDir: t.TempDir(), Workspace: ws, BaseMCP: []byte(`{"mcpServers":{}}`),
		TaskBranch: "tatara/task-x",
		Repos: []bootstrap.RepoSpec{
			{Name: "a", URL: "https://h/a", Branch: "main"},
			{Name: "b", URL: "https://h/b", Branch: "dev"},
		},
		RepoURL: "https://h/a", HookCommand: "/x", PermissionMode: "bypassPermissions",
	}
	require.NoError(t, bootstrap.Render(p, func(dir string, a ...string) error {
		calls = append(calls, append([]string{dir}, a...))
		return nil
	}))
	joined := func() string {
		var s []string
		for _, c := range calls {
			s = append(s, strings.Join(c, " "))
		}
		return strings.Join(s, "|")
	}()
	require.Contains(t, joined, "clone")
	require.Contains(t, joined, "https://h/a")
	require.Contains(t, joined, filepath.Join(ws, "a"))
	require.Contains(t, joined, "https://h/b")
	require.Contains(t, joined, filepath.Join(ws, "b"))
	// checkout the task branch inside each repo dir
	require.Contains(t, joined, filepath.Join(ws, "a")+" checkout -b tatara/task-x")
	require.Contains(t, joined, filepath.Join(ws, "b")+" checkout -b tatara/task-x")
	// session config lives in the workspace, not inside a repo
	b, _ := os.ReadFile(filepath.Join(ws, ".mcp.json"))
	require.NotEmpty(t, b)
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
	require.NoError(t, bootstrap.Render(p, func(dir string, a ...string) error { calls = append(calls, a); return nil }))

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

func TestCommitAndPushAll_PushesEachRepoOnItsDir(t *testing.T) {
	var calls [][]string
	git := func(dir string, a ...string) error {
		calls = append(calls, append([]string{dir}, a...))
		if len(a) >= 3 && a[0] == "diff" && a[1] == "--cached" && a[2] == "--quiet" {
			return errors.New("changes")
		}
		return nil
	}
	repos := []bootstrap.RepoSpec{{Name: "a"}, {Name: "b"}}
	require.NoError(t, bootstrap.CommitAndPushAll("/ws", repos, "tatara/task-x", "msg", git))
	var s []string
	for _, c := range calls {
		s = append(s, strings.Join(c, " "))
	}
	all := strings.Join(s, "|")
	require.Contains(t, all, "/ws/a push -u origin tatara/task-x")
	require.Contains(t, all, "/ws/b push -u origin tatara/task-x")
}

func TestCommitAndPush_CommitsWhenDirtyThenPushes(t *testing.T) {
	var calls [][]string
	git := func(dir string, a ...string) error {
		calls = append(calls, a)
		// `diff --cached --quiet` exits non-zero when there are staged changes.
		if len(a) >= 3 && a[0] == "diff" && a[1] == "--cached" && a[2] == "--quiet" {
			return errors.New("exit status 1")
		}
		return nil
	}
	require.NoError(t, bootstrap.CommitAndPush("/repo", "tatara/task-abc", "agent work", git))

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
	git := func(dir string, a ...string) error {
		if len(a) >= 1 && a[0] == "commit" {
			committed = true
		}
		if len(a) >= 1 && a[0] == "push" {
			pushed = true
		}
		return nil // diff --cached --quiet returns nil -> nothing staged
	}
	require.NoError(t, bootstrap.CommitAndPush("/repo", "b", "m", git))
	require.False(t, committed, "must not commit when nothing is staged")
	require.True(t, pushed, "branch must still be pushed so write-back can open the PR")
}
