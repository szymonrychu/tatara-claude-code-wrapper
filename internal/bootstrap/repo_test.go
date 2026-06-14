package bootstrap_test

import (
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
