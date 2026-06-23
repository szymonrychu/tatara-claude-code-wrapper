package bootstrap_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
)

// TestRepoDir_MatchesSingleRepoCloneDest is a regression guard for the audit
// finding that single-repo clone moved into workspace/<owner>/<repo> but the
// commit/push and hook-install consumers still targeted the workspace root.
// RepoDir is the single source of truth for the single-repo dir and MUST equal
// the directory Render clones into; an empty/invalid URL must return "".
func TestRepoDir_MatchesSingleRepoCloneDest(t *testing.T) {
	ws := t.TempDir()

	// Capture the destination Render actually clones into for a single repo.
	var cloneDest string
	fakeGit := func(dir string, args ...string) error {
		if len(args) > 0 && args[0] == "clone" {
			cloneDest = args[len(args)-1]
		}
		return nil
	}
	p := bootstrap.Params{
		HomeDir:    t.TempDir(),
		Workspace:  ws,
		BaseMCP:    []byte(`{"mcpServers":{}}`),
		RepoURL:    "https://github.com/owner/myrepo.git",
		RepoBranch: "main",
	}
	require.NoError(t, bootstrap.Render(p, fakeGit))

	require.Equal(t, cloneDest, bootstrap.RepoDir(ws, p.RepoURL),
		"RepoDir must equal the directory Render clones the single repo into")
	require.Equal(t, filepath.Join(ws, "owner", "myrepo"), bootstrap.RepoDir(ws, p.RepoURL))

	// Invalid/empty URLs must yield "" so callers skip rather than operate on
	// the workspace root.
	require.Equal(t, "", bootstrap.RepoDir(ws, ""))
	require.Equal(t, "", bootstrap.RepoDir(ws, "https://github.com/"))
	require.Equal(t, "", bootstrap.RepoDir(ws, "https://host/repo.git"),
		"single-segment (no owner) must return empty")
}

// TestCommitAndPush_UsesNoVerify asserts that CommitAndPush passes --no-verify
// to both git commit and git push (safety-net push must bypass hooks).
func TestCommitAndPush_UsesNoVerify(t *testing.T) {
	var calls [][]string

	// Fake git: "diff --cached --quiet" returns non-nil so commit proceeds.
	fakeGit := func(dir string, args ...string) error {
		calls = append(calls, args)
		// Simulate dirty tree so commit is not skipped
		if len(args) >= 2 && args[0] == "diff" && args[1] == "--cached" {
			return errFake("dirty")
		}
		return nil
	}

	pushed, err := bootstrap.CommitAndPush("/tmp/dir", "feat/x", "tatara agent: feat/x", fakeGit)
	require.NoError(t, err)
	require.True(t, pushed, "dirty tree must report pushed=true")

	commitCalls := callsContainingAll(calls, "commit")
	require.NotEmpty(t, commitCalls, "git commit must be called")
	require.True(t, argsContainAll("--no-verify")(commitCalls[0]),
		"git commit must include --no-verify, got: %v", commitCalls[0])

	pushCalls := callsContainingAll(calls, "push")
	require.NotEmpty(t, pushCalls, "git push must be called")
	require.True(t, argsContainAll("--no-verify")(pushCalls[0]),
		"git push must include --no-verify, got: %v", pushCalls[0])
}

// TestCommitAndPushAll_SkipsEmptyNamespace asserts that CommitAndPushAll skips
// repos whose URL yields an empty namespace rather than committing into the
// workspace root (finding 3).
func TestCommitAndPushAll_SkipsEmptyNamespace(t *testing.T) {
	var calls [][]string
	fakeGit := func(dir string, args ...string) error {
		calls = append(calls, args)
		if len(args) >= 2 && args[0] == "diff" && args[1] == "--cached" {
			return errFake("dirty")
		}
		return nil
	}

	repos := []bootstrap.RepoSpec{
		{Name: "good", URL: "https://github.com/owner/repo"},
		{Name: "bad-empty", URL: ""},                   // namespacePath returns ""
		{Name: "bad-host", URL: "https://github.com/"}, // single-segment -> ""
	}
	pushedRepos, err := bootstrap.CommitAndPushAll("/tmp/ws", repos, "feat/x", "msg", fakeGit)
	require.NoError(t, err)
	require.Equal(t, []string{"good"}, pushedRepos, "only the valid-namespace repo must be reported pushed")

	// Only the good repo should have had git add/commit/push
	addCalls := callsContainingAll(calls, "add")
	require.Len(t, addCalls, 1, "git add must run exactly once (for good repo only)")
}

// TestCloneRepo_SkipsWhenGitDirExists asserts that Render skips clone when the
// repo's namespace subdir already contains a .git directory (pod restart
// resume, finding 6). The .git dir is at workspace/owner/repo/.git, not at
// workspace root, because single-repo mode now clones into a namespace subdir.
func TestCloneRepo_SkipsWhenGitDirExists(t *testing.T) {
	ws := t.TempDir()
	// Pre-create a .git marker at the namespace-subdir path for
	// "https://github.com/x/y" to simulate an already-cloned workspace.
	if err := os.MkdirAll(filepath.Join(ws, "x", "y", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	var cloneCalled bool
	fakeGit := func(dir string, args ...string) error {
		for _, a := range args {
			if a == "clone" {
				cloneCalled = true
			}
		}
		return nil
	}

	p := bootstrap.Params{
		HomeDir:        t.TempDir(),
		Workspace:      ws,
		BaseMCP:        []byte(`{"mcpServers":{}}`),
		RepoURL:        "https://github.com/x/y",
		RepoBranch:     "main",
		HookCommand:    "/usr/local/bin/cc-stop-hook",
		PermissionMode: "bypassPermissions",
	}
	require.NoError(t, bootstrap.Render(p, fakeGit))
	require.False(t, cloneCalled, "git clone must not be called when .git already exists in workspace")
}

// TestCommitAndPush_CleanTree_NoPush asserts that a clean tree (diff exits 0)
// skips both commit and push.
func TestCommitAndPush_CleanTree_NoPush(t *testing.T) {
	var calls [][]string

	// Fake git: "diff --cached --quiet" returns nil -> clean tree
	fakeGit := func(dir string, args ...string) error {
		calls = append(calls, args)
		return nil // all succeed, including diff -> clean
	}

	pushed, err := bootstrap.CommitAndPush("/tmp/dir", "feat/x", "msg", fakeGit)
	require.NoError(t, err)
	require.False(t, pushed, "clean tree must report pushed=false")

	commitCalls := callsContainingAll(calls, "commit")
	require.Empty(t, commitCalls, "commit must be skipped on clean tree")

	pushCalls := callsContainingAll(calls, "push")
	require.Empty(t, pushCalls, "push must be skipped on clean tree")
}

// TestCommitAndPushAll_ReturnsOnlyDirtyRepos asserts the returned list contains
// exactly the repos that had a diff and pushed; a clean repo is omitted.
func TestCommitAndPushAll_ReturnsOnlyDirtyRepos(t *testing.T) {
	// dirtyByDir[dir]=true makes "diff --cached --quiet" report dirty for that dir.
	dirtyByDir := map[string]bool{
		filepath.Join("/tmp/ws", "owner", "dirtyrepo"): true,
	}
	fakeGit := func(dir string, args ...string) error {
		if len(args) >= 2 && args[0] == "diff" && args[1] == "--cached" {
			if dirtyByDir[dir] {
				return errFake("dirty")
			}
			return nil // clean
		}
		return nil
	}

	repos := []bootstrap.RepoSpec{
		{Name: "dirty", URL: "https://github.com/owner/dirtyrepo"},
		{Name: "clean", URL: "https://github.com/owner/cleanrepo"},
	}
	pushed, err := bootstrap.CommitAndPushAll("/tmp/ws", repos, "feat/x", "msg", fakeGit)
	require.NoError(t, err)
	require.Equal(t, []string{"dirty"}, pushed)
}
