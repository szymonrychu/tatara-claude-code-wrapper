package bootstrap_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
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
	bootstrap.InstallHooks(workspace, nil, "https://github.com/x/y", fakeCmd, nil, nil)

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
	bootstrap.InstallHooks(workspace, repos, "", fakeCmd, nil, nil)

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
	bootstrap.InstallHooks(workspace, nil, "https://github.com/x/y", fakeCmd, nil, nil)

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
	bootstrap.InstallHooks("/tmp/ws", nil, "", fakeCmd, nil, nil)
	require.False(t, called, "no commands must run when no repo is configured")
}

// TestInstallHooks_MetricOk verifies that a successful hook install increments
// BootstrapHookInstall with result=ok for both mise and pre-commit.
func TestInstallHooks_MetricOk(t *testing.T) {
	fakeCmd := func(dir, name string, args ...string) error { return nil }
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	bootstrap.InstallHooks("/tmp/ws", nil, "https://github.com/x/y", fakeCmd, slog.New(slog.NewTextHandler(io.Discard, nil)), m)

	mfs, err := reg.Gather()
	require.NoError(t, err)
	var total float64
	for _, mf := range mfs {
		if mf.GetName() == "ccw_bootstrap_hook_install_total" {
			for _, metric := range mf.GetMetric() {
				total += metric.GetCounter().GetValue()
			}
		}
	}
	require.Equal(t, float64(2), total, "expected 2 ok increments (mise + pre-commit)")
}

// TestInstallHooks_MetricFail verifies that a failing hook install increments
// BootstrapHookInstall with result=fail and still attempts the next command.
func TestInstallHooks_MetricFail(t *testing.T) {
	fakeCmd := func(dir, name string, args ...string) error {
		if name == "mise" {
			return errFake("mise not found")
		}
		return nil
	}
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	bootstrap.InstallHooks("/tmp/ws", nil, "https://github.com/x/y", fakeCmd, slog.New(slog.NewTextHandler(io.Discard, nil)), m)

	mfs, err := reg.Gather()
	require.NoError(t, err)
	okCount := float64(0)
	failCount := float64(0)
	for _, mf := range mfs {
		if mf.GetName() == "ccw_bootstrap_hook_install_total" {
			for _, metric := range mf.GetMetric() {
				for _, lp := range metric.GetLabel() {
					if lp.GetName() == "result" {
						if lp.GetValue() == "ok" {
							okCount += metric.GetCounter().GetValue()
						} else if lp.GetValue() == "fail" {
							failCount += metric.GetCounter().GetValue()
						}
					}
				}
			}
		}
	}
	require.Equal(t, float64(1), failCount, "expected 1 fail increment (mise)")
	require.Equal(t, float64(1), okCount, "expected 1 ok increment (pre-commit)")
}

// TestInstallHooks_LogsViaInjectedLogger verifies that InstallHooks uses the
// injected *slog.Logger rather than the package-global slog (finding 1).
func TestInstallHooks_LogsViaInjectedLogger(t *testing.T) {
	fakeCmd := func(dir, name string, args ...string) error {
		if name == "mise" {
			return errFake("mise not found")
		}
		return nil
	}
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	bootstrap.InstallHooks("/tmp/ws", nil, "https://github.com/x/y", fakeCmd, log, nil)

	// Expect at least one structured log line with action=hook_install
	lines := bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n"))
	found := false
	for _, ln := range lines {
		var rec map[string]any
		if json.Unmarshal(ln, &rec) == nil && rec["action"] == "hook_install" {
			found = true
			break
		}
	}
	require.True(t, found, "no hook_install log line emitted by injected logger")
}

// errFake is a simple error type for tests.
type errFake string

func (e errFake) Error() string { return string(e) }
