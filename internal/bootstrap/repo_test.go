package bootstrap_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
)

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

	err := bootstrap.CommitAndPush("/tmp/dir", "feat/x", "tatara agent: feat/x", fakeGit)
	require.NoError(t, err)

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
	err := bootstrap.CommitAndPushAll("/tmp/ws", repos, "feat/x", "msg", fakeGit)
	require.NoError(t, err)

	// Only the good repo should have had git add/commit/push
	addCalls := callsContainingAll(calls, "add")
	require.Len(t, addCalls, 1, "git add must run exactly once (for good repo only)")
}

// TestCloneRepo_SkipsWhenGitDirExists asserts that cloneRepo is a no-op when
// the workspace already contains a .git directory (pod restart resume, finding 6).
func TestCloneRepo_SkipsWhenGitDirExists(t *testing.T) {
	ws := t.TempDir()
	// Pre-create a .git marker to simulate an already-cloned workspace.
	if err := os.MkdirAll(filepath.Join(ws, ".git"), 0o755); err != nil {
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

	err := bootstrap.CommitAndPush("/tmp/dir", "feat/x", "msg", fakeGit)
	require.NoError(t, err)

	commitCalls := callsContainingAll(calls, "commit")
	require.Empty(t, commitCalls, "commit must be skipped on clean tree")

	pushCalls := callsContainingAll(calls, "push")
	require.Empty(t, pushCalls, "push must be skipped on clean tree")
}
