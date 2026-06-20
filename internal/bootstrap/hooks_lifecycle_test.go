package bootstrap_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
)

// recordedHook captures one HookRunner invocation.
type recordedHook struct {
	dir, command string
	posArgs      []string
	extraEnv     []string
}

// recordingHookRunner returns a HookRunner that appends each call to *calls and
// returns retErr.
func recordingHookRunner(calls *[]recordedHook, retErr error) bootstrap.HookRunner {
	return func(dir, command string, posArgs, extraEnv []string) error {
		*calls = append(*calls, recordedHook{dir: dir, command: command, posArgs: posArgs, extraEnv: extraEnv})
		return retErr
	}
}

func TestRunHook_SkipsWhenCommandEmptyOrRunnerNil(t *testing.T) {
	var calls []recordedHook
	run := recordingHookRunner(&calls, nil)

	// Empty command: runner must not be invoked.
	bootstrap.RunHook("preClone", "", "/ws", []string{"a"}, nil, run, nil, nil)
	require.Empty(t, calls, "empty command must skip the runner")

	// Nil runner: must be a safe no-op (no panic).
	bootstrap.RunHook("preClone", "echo hi", "/ws", nil, nil, nil, nil, nil)
}

func TestRunHook_PassesArgsAndEnv(t *testing.T) {
	var calls []recordedHook
	run := recordingHookRunner(&calls, nil)

	bootstrap.RunHook("postClone", "echo $1", "/ws", []string{"/dest"}, []string{"TATARA_HOOK_CLONE_DEST=/dest"}, run, nil, nil)

	require.Len(t, calls, 1)
	require.Equal(t, "/ws", calls[0].dir)
	require.Equal(t, "echo $1", calls[0].command)
	require.Equal(t, []string{"/dest"}, calls[0].posArgs)
	require.Equal(t, []string{"TATARA_HOOK_CLONE_DEST=/dest"}, calls[0].extraEnv)
}

func TestRunHook_FailureNeverPanicsOrReturns(t *testing.T) {
	var calls []recordedHook
	run := recordingHookRunner(&calls, errors.New("boom"))
	// RunHook returns nothing; a failing hook must not panic.
	bootstrap.RunHook("preClone", "false", "/ws", nil, nil, run, nil, nil)
	require.Len(t, calls, 1)
}

// TestDefaultHookRunner_ExecutesWithArgAndEnv verifies the production runner
// actually runs `sh -c`, exposes the first positional arg as $1, and the
// extraEnv to the process.
func TestDefaultHookRunner_ExecutesWithArgAndEnv(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	// Write "$1|$TATARA_HOOK_REPO_URL" to out.txt.
	cmd := `printf '%s|%s' "$1" "$TATARA_HOOK_REPO_URL" > ` + out
	err := bootstrap.DefaultHookRunner(dir, cmd, []string{"http://repo"}, []string{"TATARA_HOOK_REPO_URL=http://repo"})
	require.NoError(t, err)
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, "http://repo|http://repo", string(b))
}

func TestDefaultHookRunner_NonZeroExitReturnsError(t *testing.T) {
	require.Error(t, bootstrap.DefaultHookRunner(t.TempDir(), "exit 3", nil, nil))
}

// TestRender_FiresCloneHooks_SingleRepo asserts preClone fires with the repo URL
// and postClone with the clone dest, both carrying the right env, on the
// single-repo path.
func TestRender_FiresCloneHooks_SingleRepo(t *testing.T) {
	ws := t.TempDir()
	var calls []recordedHook
	p := bootstrap.Params{
		HomeDir:       t.TempDir(),
		Workspace:     ws,
		BaseMCP:       []byte(`{"mcpServers":{}}`),
		RepoURL:       "https://github.com/owner/primary.git",
		RepoBranch:    "main",
		HookPreClone:  "echo pre",
		HookPostClone: "echo post",
		HookRun:       recordingHookRunner(&calls, nil),
	}
	require.NoError(t, bootstrap.Render(p, func(string, ...string) error { return nil }))

	require.Len(t, calls, 2)
	require.Equal(t, "echo pre", calls[0].command)
	require.Equal(t, []string{p.RepoURL}, calls[0].posArgs)
	require.Equal(t, []string{"TATARA_HOOK_REPO_URL=" + p.RepoURL}, calls[0].extraEnv)

	wantDest := filepath.Join(ws, "owner", "primary")
	require.Equal(t, "echo post", calls[1].command)
	require.Equal(t, []string{wantDest}, calls[1].posArgs)
	require.Equal(t, []string{"TATARA_HOOK_CLONE_DEST=" + wantDest}, calls[1].extraEnv)
}

// TestRender_FiresCloneHooks_MultiRepo asserts the hooks fire once per repo on
// the multi-repo path.
func TestRender_FiresCloneHooks_MultiRepo(t *testing.T) {
	ws := t.TempDir()
	var calls []recordedHook
	p := bootstrap.Params{
		HomeDir:       t.TempDir(),
		Workspace:     ws,
		BaseMCP:       []byte(`{"mcpServers":{}}`),
		HookPreClone:  "echo pre",
		HookPostClone: "echo post",
		HookRun:       recordingHookRunner(&calls, nil),
		Repos: []bootstrap.RepoSpec{
			{Name: "a", URL: "https://github.com/owner/a.git", Branch: "main"},
			{Name: "b", URL: "https://github.com/owner/b.git", Branch: "main"},
		},
	}
	require.NoError(t, bootstrap.Render(p, func(string, ...string) error { return nil }))

	// Two repos x (preClone + postClone) = 4 calls.
	require.Len(t, calls, 4)
	var pre, post int
	for _, c := range calls {
		switch c.command {
		case "echo pre":
			pre++
			require.Len(t, c.posArgs, 1)
			require.Contains(t, c.extraEnv[0], "TATARA_HOOK_REPO_URL=")
		case "echo post":
			post++
			require.Contains(t, c.extraEnv[0], "TATARA_HOOK_CLONE_DEST=")
		}
	}
	require.Equal(t, 2, pre)
	require.Equal(t, 2, post)
}

// TestRender_HookFailureDoesNotAbort asserts a failing clone hook never breaks
// the bootstrap.
func TestRender_HookFailureDoesNotAbort(t *testing.T) {
	ws := t.TempDir()
	var calls []recordedHook
	p := bootstrap.Params{
		HomeDir:       t.TempDir(),
		Workspace:     ws,
		BaseMCP:       []byte(`{"mcpServers":{}}`),
		RepoURL:       "https://github.com/owner/primary.git",
		RepoBranch:    "main",
		HookPreClone:  "false",
		HookPostClone: "false",
		HookRun:       recordingHookRunner(&calls, errors.New("hook failed")),
	}
	require.NoError(t, bootstrap.Render(p, func(string, ...string) error { return nil }))
	require.Len(t, calls, 2, "both hooks attempted despite failures")
}

// TestRender_NoHookCommands_NotFired asserts that with no hook commands the
// runner is never invoked even when wired.
func TestRender_NoHookCommands_NotFired(t *testing.T) {
	ws := t.TempDir()
	var calls []recordedHook
	p := bootstrap.Params{
		HomeDir:    t.TempDir(),
		Workspace:  ws,
		BaseMCP:    []byte(`{"mcpServers":{}}`),
		RepoURL:    "https://github.com/owner/primary.git",
		RepoBranch: "main",
		HookRun:    recordingHookRunner(&calls, nil),
	}
	require.NoError(t, bootstrap.Render(p, func(string, ...string) error { return nil }))
	require.Empty(t, calls, "no hook commands -> runner never called")
}
