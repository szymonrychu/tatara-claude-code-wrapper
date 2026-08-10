package bootstrap_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
)

// TestPushOnly_PushesWithoutStagingOrCommitting is the load-bearing assertion
// for the mid-turn push safety net: it must move the branch to origin and do
// nothing else. Any `git add` or `git commit` here would take .git/index.lock
// out from under the agent's own concurrent git calls, and could publish a
// half-edited tree.
func TestPushOnly_PushesWithoutStagingOrCommitting(t *testing.T) {
	var calls [][]string
	fakeGit := func(dir string, args ...string) error {
		calls = append(calls, args)
		return nil
	}

	require.NoError(t, bootstrap.PushOnly("/tmp/dir", "feat/x", fakeGit))

	require.Len(t, calls, 1, "PushOnly must issue exactly one git call")
	require.Equal(t, []string{"push", "--no-verify", "--quiet", "origin", "feat/x"}, calls[0])
	require.Empty(t, callsContainingAll(calls, "add"), "the safety net must never stage")
	require.Empty(t, callsContainingAll(calls, "commit"), "the safety net must never commit")
}

// TestPushOnly_NothingToPushIsNotAnError covers the common case: the agent has
// made no new commit since the last tick. Real git exits 0 and prints
// "Everything up-to-date"; a runner that surfaces that text as an error must
// also be treated as success.
func TestPushOnly_NothingToPushIsNotAnError(t *testing.T) {
	require.NoError(t, bootstrap.PushOnly("/tmp/dir", "feat/x",
		func(string, ...string) error { return nil }))

	upToDate := func(string, ...string) error { return errFake("Everything up-to-date") }
	require.NoError(t, bootstrap.PushOnly("/tmp/dir", "feat/x", upToDate))
}

// TestPushOnly_NonFastForwardIsNotAnError: the remote moved under us. That is
// the next turn's reconcile problem (bootstrap merges the base branch on
// resume), never the safety net's, so it must not surface as an error.
func TestPushOnly_NonFastForwardIsNotAnError(t *testing.T) {
	rejected := func(string, ...string) error {
		return errFake("! [rejected]        feat/x -> feat/x (non-fast-forward)")
	}
	require.NoError(t, bootstrap.PushOnly("/tmp/dir", "feat/x", rejected))

	fetchFirst := func(string, ...string) error {
		return errFake("! [rejected] feat/x -> feat/x (fetch first)")
	}
	require.NoError(t, bootstrap.PushOnly("/tmp/dir", "feat/x", fetchFirst))
}

// TestPushOnly_RealFailureIsReturned keeps the classification honest: only the
// known-benign push outcomes are swallowed, everything else is reported so the
// caller can count and log it.
func TestPushOnly_RealFailureIsReturned(t *testing.T) {
	boom := func(string, ...string) error {
		return errFake("fatal: could not read from remote repository")
	}
	require.Error(t, bootstrap.PushOnly("/tmp/dir", "feat/x", boom))
}
