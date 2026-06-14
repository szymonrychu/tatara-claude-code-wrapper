package bootstrap

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// recordingGit records every git invocation and lets the test script the
// exit of `diff --cached --quiet` (clean tree = exit 0 = nil error).
type recordingGit struct {
	calls     [][]string
	treeDirty bool // when true, `diff --cached --quiet` returns an error (staged changes)
}

func (g *recordingGit) run(dir string, args ...string) error {
	g.calls = append(g.calls, args)
	if len(args) >= 2 && args[0] == "diff" && args[1] == "--cached" {
		if g.treeDirty {
			return errExit
		}
		return nil
	}
	return nil
}

var errExit = &gitExitError{}

type gitExitError struct{}

func (*gitExitError) Error() string { return "exit status 1" }

func didCall(calls [][]string, verb string) bool {
	for _, c := range calls {
		if len(c) > 0 && c[0] == verb {
			return true
		}
	}
	return false
}

func TestCommitAndPush_CleanTree_SkipsCommitAndPush(t *testing.T) {
	g := &recordingGit{treeDirty: false}
	err := CommitAndPush("/repo", "tatara/task-x", "msg", g.run)
	require.NoError(t, err)
	require.True(t, didCall(g.calls, "add"), "must always stage")
	require.False(t, didCall(g.calls, "commit"), "clean tree must not commit")
	require.False(t, didCall(g.calls, "push"), "clean tree must not push (no empty branch)")
}

func TestCommitAndPush_DirtyTree_CommitsAndPushes(t *testing.T) {
	g := &recordingGit{treeDirty: true}
	err := CommitAndPush("/repo", "tatara/task-x", "msg", g.run)
	require.NoError(t, err)
	require.True(t, didCall(g.calls, "commit"), "dirty tree must commit")
	require.True(t, didCall(g.calls, "push"), "dirty tree must push")
}
