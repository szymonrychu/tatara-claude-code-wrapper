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

func TestRender_ClonesEachRepoIntoNamespaceSubdirAndChecksOutBranch(t *testing.T) {
	ws := t.TempDir()
	var calls [][]string // dir + args
	p := bootstrap.Params{
		HomeDir: t.TempDir(), Workspace: ws, BaseMCP: []byte(`{"mcpServers":{}}`),
		TaskBranch: "tatara/task-x",
		Repos: []bootstrap.RepoSpec{
			{Name: "tatara-cli", URL: "https://github.com/szymonrychu/tatara-cli.git", Branch: "main"},
			{Name: "helmfile", URL: "https://gitlab.com/szymonrychu/infra/helmfile.git", Branch: "dev"},
		},
		RepoURL: "https://github.com/szymonrychu/tatara-cli.git", HookCommand: "/x", PermissionMode: "bypassPermissions",
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

	destA := filepath.Join(ws, "szymonrychu", "tatara-cli")
	destB := filepath.Join(ws, "szymonrychu", "infra", "helmfile")

	// cloned into the namespace-preserving destinations
	require.Contains(t, joined, "clone")
	require.Contains(t, joined, "https://github.com/szymonrychu/tatara-cli.git")
	require.Contains(t, joined, destA)
	require.Contains(t, joined, "https://gitlab.com/szymonrychu/infra/helmfile.git")
	require.Contains(t, joined, destB)
	// The fake GitRunner returns nil for ls-remote, so the resume path is taken:
	// fetch --unshallow, fetch origin <branch>, checkout -B <branch> FETCH_HEAD.
	require.Contains(t, joined, destA+" checkout -B tatara/task-x FETCH_HEAD")
	require.Contains(t, joined, destB+" checkout -B tatara/task-x FETCH_HEAD")
	// parent dirs of the namespace destinations were created
	require.DirExists(t, filepath.Join(ws, "szymonrychu"))
	require.DirExists(t, filepath.Join(ws, "szymonrychu", "infra"))
	// session config lives in the workspace root, not inside a repo
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

func TestRender_ChecksOutCheckoutBranchWhenNoTaskBranch(t *testing.T) {
	// MR review (issue #114 decision 4): no TaskBranch (so the turn finaliser
	// never pushes), but CheckoutBranch is the PR head and must be checked out.
	var calls [][]string
	p := bootstrap.Params{
		HomeDir: t.TempDir(), Workspace: t.TempDir(),
		BaseMCP:        []byte(`{"mcpServers":{}}`),
		RepoURL:        "https://github.com/x/y",
		RepoBranch:     "main",
		CheckoutBranch: "feature/user-pr",
		HookCommand:    "/usr/local/bin/cc-stop-hook", PermissionMode: "bypassPermissions",
	}
	require.NoError(t, bootstrap.Render(p, func(dir string, a ...string) error { calls = append(calls, a); return nil }))

	var checkedOut bool
	for _, c := range calls {
		j := strings.Join(c, " ")
		if strings.Contains(j, "checkout") && strings.Contains(j, "feature/user-pr") {
			checkedOut = true
		}
	}
	require.True(t, checkedOut, "CheckoutBranch (PR head) must be checked out when TaskBranch is empty")
}

func TestCommitAndPushAll_PushesEachRepoOnItsNamespaceDir(t *testing.T) {
	var calls [][]string
	git := func(dir string, a ...string) error {
		calls = append(calls, append([]string{dir}, a...))
		if len(a) >= 3 && a[0] == "diff" && a[1] == "--cached" && a[2] == "--quiet" {
			return errors.New("changes")
		}
		return nil
	}
	repos := []bootstrap.RepoSpec{
		{Name: "tatara-cli", URL: "https://github.com/szymonrychu/tatara-cli.git"},
		{Name: "helmfile", URL: "https://gitlab.com/szymonrychu/infra/helmfile.git"},
	}
	require.NoError(t, bootstrap.CommitAndPushAll("/ws", repos, "tatara/task-x", "msg", git))
	var s []string
	for _, c := range calls {
		s = append(s, strings.Join(c, " "))
	}
	all := strings.Join(s, "|")
	require.Contains(t, all, "/ws/szymonrychu/tatara-cli push --no-verify -u origin tatara/task-x")
	require.Contains(t, all, "/ws/szymonrychu/infra/helmfile push --no-verify -u origin tatara/task-x")
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
	require.Contains(t, joined, "commit --no-verify -m agent work")
	require.Contains(t, joined, "push --no-verify -u origin tatara/task-abc")
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
	require.False(t, pushed, "must not push on a clean tree (no empty branch created)")
}

func TestRender_SessionConfigStaysAtWorkspaceRootWithNamespaceClones(t *testing.T) {
	ws := t.TempDir()
	p := bootstrap.Params{
		HomeDir: t.TempDir(), Workspace: ws, BaseMCP: []byte(`{"mcpServers":{}}`),
		ProjectClaudeMd: "PROJECT RULES",
		TaskBranch:      "tatara/task-x",
		Repos: []bootstrap.RepoSpec{
			{Name: "tatara-cli", URL: "https://github.com/szymonrychu/tatara-cli.git", Branch: "main"},
		},
		RepoURL: "https://github.com/szymonrychu/tatara-cli.git", HookCommand: "/x", PermissionMode: "bypassPermissions",
	}
	require.NoError(t, bootstrap.Render(p, func(dir string, a ...string) error { return nil }))

	// config at workspace root
	require.FileExists(t, filepath.Join(ws, ".mcp.json"))
	require.FileExists(t, filepath.Join(ws, "CLAUDE.md"))
	b, _ := os.ReadFile(filepath.Join(ws, "CLAUDE.md"))
	require.Equal(t, "PROJECT RULES", string(b))

	// config is NOT duplicated inside the repo namespace subdir
	require.NoFileExists(t, filepath.Join(ws, "szymonrychu", "tatara-cli", ".mcp.json"))
	require.NoFileExists(t, filepath.Join(ws, "szymonrychu", "tatara-cli", "CLAUDE.md"))
}
