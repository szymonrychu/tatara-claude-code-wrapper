package bootstrap_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
)

// nineRepos is the shape every task actually gets: the operator sets
// TATARA_REPOS to the whole enrolled repo set and merely puts the Task's own
// repo first, so there is no single-repo loop once a Project has more than one
// Repository (tatara-operator/internal/agent/pod.go).
func nineRepos() []bootstrap.RepoSpec {
	repos := make([]bootstrap.RepoSpec, 0, 9)
	for i := range 9 {
		repos = append(repos, bootstrap.RepoSpec{
			Name: fmt.Sprintf("r%d", i),
			URL:  fmt.Sprintf("https://github.com/szymonrychu/r%d.git", i),
		})
	}
	return repos
}

// dirtyGitFailingPushIn is a GitRunner where every repo has staged changes and
// the named dirs fail their push.
func dirtyGitFailingPushIn(calls *[][]string, failDirs ...string) bootstrap.GitRunner {
	return func(dir string, a ...string) error {
		*calls = append(*calls, append([]string{dir}, a...))
		if len(a) >= 3 && a[0] == "diff" && a[1] == "--cached" && a[2] == "--quiet" {
			return errors.New("changes staged")
		}
		if a[0] == "push" {
			for _, d := range failDirs {
				if strings.Contains(dir, d) {
					return errors.New("exit status 1")
				}
			}
		}
		return nil
	}
}

// ONE FAILING REPO MUST NOT COST THE OTHER EIGHT THEIR COMMIT.
//
// The loop used to return on the first error, so every repo after it never even
// reached `git add -A`. The mid-turn safety net cannot cover for that: PushOnly
// deliberately omits add/commit, so a tree that was never committed has nothing
// to push, and /workspace is the container writable layer with no volume. Those
// edits died with the pod.
func TestCommitAndPushAll_AFailingRepoDoesNotSkipTheRestOfTheFanOut(t *testing.T) {
	var calls [][]string
	git := dirtyGitFailingPushIn(&calls, "/r3")

	pushed, failed, err := bootstrap.CommitAndPushAll("/ws", nineRepos(), "tatara/task-x", "msg", git, nil, nil)

	require.Error(t, err, "the failure is still reported; it just stops aborting the loop")
	require.Contains(t, err.Error(), "r3")

	for i := range 9 {
		dir := fmt.Sprintf("/ws/szymonrychu/r%d", i)
		require.True(t, hasCall(calls, dir, "add", "-A"),
			"repo %d never reached `git add -A`: its work would die with the pod", i)
		require.True(t, hasCall(calls, dir, "commit"),
			"repo %d was never committed", i)
	}

	require.Equal(t, []string{"r0", "r1", "r2", "r4", "r5", "r6", "r7", "r8"}, pushed)
	require.Equal(t, []string{"r3"}, failed,
		"a short pushed list is indistinguishable from a clean tree; the failed set is what says otherwise")
}

// Every failure is named and every failure is joined: with the loop no longer
// aborting, the error is a REPORT, and a report that stops at the first entry is
// the same blind spot in a different place.
func TestCommitAndPushAll_JoinsEveryFailureAndNamesEveryFailedRepo(t *testing.T) {
	var calls [][]string
	git := dirtyGitFailingPushIn(&calls, "/r1", "/r7")

	pushed, failed, err := bootstrap.CommitAndPushAll("/ws", nineRepos(), "tatara/task-x", "msg", git, nil, nil)

	require.Error(t, err)
	require.Contains(t, err.Error(), "r1")
	require.Contains(t, err.Error(), "r7")
	require.Equal(t, []string{"r1", "r7"}, failed)
	require.NotContains(t, pushed, "r1")
	require.NotContains(t, pushed, "r7")
	require.Len(t, pushed, 7)
}

// A clean repo is neither pushed nor failed: "nothing to push" is not an error,
// and reporting it as one would make the failed set meaningless.
func TestCommitAndPushAll_CleanRepoIsNeitherPushedNorFailed(t *testing.T) {
	git := func(dir string, a ...string) error { return nil } // diff --cached --quiet == clean

	pushed, failed, err := bootstrap.CommitAndPushAll("/ws", nineRepos(), "tatara/task-x", "msg", git, nil, nil)

	require.NoError(t, err)
	require.Empty(t, pushed)
	require.Empty(t, failed)
}

func hasCall(calls [][]string, dir string, args ...string) bool {
	for _, c := range calls {
		if c[0] != dir || len(c) < len(args)+1 {
			continue
		}
		match := true
		for i, a := range args {
			if c[i+1] != a {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
