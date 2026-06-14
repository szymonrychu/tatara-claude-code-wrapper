package bootstrap_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
)

// TestInstallHooks_SingleRepo asserts that InstallHooks calls mise install and
// pre-commit install --hook-type pre-commit --hook-type pre-push in the
// workspace dir when repos is empty and repoURL is set.
func TestInstallHooks_SingleRepo(t *testing.T) {
	type call struct {
		dir  string
		name string
		args []string
	}
	var calls []call

	fakeCmd := func(dir, name string, args ...string) error {
		calls = append(calls, call{dir: dir, name: name, args: args})
		return nil
	}

	workspace := "/tmp/ws"
	bootstrap.InstallHooks(workspace, nil, "https://github.com/x/y", fakeCmd)

	require.Len(t, calls, 2, "expected 2 calls: mise install + pre-commit install")

	miseCall := calls[0]
	require.Equal(t, workspace, miseCall.dir)
	require.Equal(t, "mise", miseCall.name)
	require.Equal(t, []string{"install"}, miseCall.args)

	pcCall := calls[1]
	require.Equal(t, workspace, pcCall.dir)
	require.Equal(t, "pre-commit", pcCall.name)
	require.Equal(t, []string{"install", "--hook-type", "pre-commit", "--hook-type", "pre-push"}, pcCall.args)
}

// TestInstallHooks_MultiRepo asserts that InstallHooks calls mise+pre-commit
// in the correct namespaced subdir for each repo in the list.
func TestInstallHooks_MultiRepo(t *testing.T) {
	type call struct {
		dir  string
		name string
	}
	var calls []call

	fakeCmd := func(dir, name string, args ...string) error {
		calls = append(calls, call{dir: dir, name: name})
		return nil
	}

	workspace := "/tmp/ws"
	repos := []bootstrap.RepoSpec{
		{Name: "cli", URL: "https://github.com/owner/tatara-cli"},
		{Name: "memory", URL: "https://github.com/owner/tatara-memory"},
	}
	bootstrap.InstallHooks(workspace, repos, "", fakeCmd)

	// 2 repos * 2 commands = 4 calls
	require.Len(t, calls, 4)

	require.Equal(t, "/tmp/ws/owner/tatara-cli", calls[0].dir)
	require.Equal(t, "mise", calls[0].name)
	require.Equal(t, "/tmp/ws/owner/tatara-cli", calls[1].dir)
	require.Equal(t, "pre-commit", calls[1].name)
	require.Equal(t, "/tmp/ws/owner/tatara-memory", calls[2].dir)
	require.Equal(t, "mise", calls[2].name)
	require.Equal(t, "/tmp/ws/owner/tatara-memory", calls[3].dir)
	require.Equal(t, "pre-commit", calls[3].name)
}

// TestInstallHooks_BestEffort asserts that a failure in mise install or
// pre-commit install does NOT abort; remaining repos/commands still run.
func TestInstallHooks_BestEffort(t *testing.T) {
	type call struct {
		dir  string
		name string
	}
	var calls []call
	fakeCmd := func(dir, name string, args ...string) error {
		calls = append(calls, call{dir: dir, name: name})
		// Always fail mise install to simulate missing mise
		if name == "mise" {
			return errFake("mise not found")
		}
		return nil
	}

	workspace := "/tmp/ws"
	// Must not panic or return error - InstallHooks is void
	bootstrap.InstallHooks(workspace, nil, "https://github.com/x/y", fakeCmd)

	// Even though mise failed, pre-commit install must still be attempted
	require.Len(t, calls, 2, "both calls must be attempted even when mise fails")
	require.Equal(t, "mise", calls[0].name)
	require.Equal(t, "pre-commit", calls[1].name)
}

// TestInstallHooks_NoRepo asserts that InstallHooks is a no-op when both
// repos and repoURL are empty.
func TestInstallHooks_NoRepo(t *testing.T) {
	var called bool
	fakeCmd := func(dir, name string, args ...string) error {
		called = true
		return nil
	}
	bootstrap.InstallHooks("/tmp/ws", nil, "", fakeCmd)
	require.False(t, called, "no commands must run when no repo is configured")
}

// errFake is a simple error type for tests.
type errFake string

func (e errFake) Error() string { return string(e) }
