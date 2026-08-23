package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/promptguard"
)

// liveManifest is a tool-manifest.json body advertising exactly the tool names
// this repo's agent-facing prompt text references today. The guard's whole job
// is comparing the scan against a manifest, so the fixture is written from the
// scan rather than hardcoded - a hardcoded pair would go stale the moment a
// prompt block names a third tool, and would do it by passing.
func liveManifest(t *testing.T) string {
	t.Helper()
	root, err := promptguard.RepoRoot()
	require.NoError(t, err)
	lits, err := promptguard.Literals(root)
	require.NoError(t, err)

	body := `{"tools":[`
	for i, n := range promptguard.ToolNames(lits) {
		if i > 0 {
			body += ","
		}
		body += `{"name":"` + n + `"}`
	}
	body += `]}`

	path := filepath.Join(t.TempDir(), "tool-manifest.json")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

// liveAgentsDir is an agents directory holding exactly the roster the injected
// CLAUDE.md advertises, i.e. the real clone shape: flat *.md at the top level.
func liveAgentsDir(t *testing.T) string {
	t.Helper()
	roster, err := bootstrap.DelegationRoster()
	require.NoError(t, err)
	dir := t.TempDir()
	for _, n := range roster {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n+".md"), []byte("# agent"), 0o644))
	}
	return dir
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := promptguard.RepoRoot()
	require.NoError(t, err)
	return root
}

func TestRun_PassesWhenBothOraclesAgree(t *testing.T) {
	require.NoError(t, run(config{
		root:         repoRoot(t),
		manifestPath: liveManifest(t),
		agentsDir:    liveAgentsDir(t),
	}))
}

// The #136 shape: a name in agent-facing prompt text that the pinned cli no
// longer advertises. The manifest here is a real manifest; it is simply missing
// one name the prompt text uses.
func TestRun_FailsOnAPromptToolNameTheManifestDoesNotAdvertise(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool-manifest.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"tools":[{"name":"submit_outcome"}]}`), 0o644))

	err := run(config{root: repoRoot(t), manifestPath: path, agentsDir: liveAgentsDir(t)})
	require.Error(t, err)
	require.Contains(t, err.Error(), "issue_write")
	// The message must point at the prose to fix, not just name the tool.
	require.Contains(t, err.Error(), "internal/bootstrap/bootstrap.go")
}

// The C1 shape, in its live form: the skills plugin ships an agent the injected
// CLAUDE.md never names, so it lands in the pod and no agent knows to dispatch
// it.
func TestRun_FailsOnAnInstalledAgentTheRosterDoesNotName(t *testing.T) {
	dir := liveAgentsDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "surveyor.md"), []byte("# agent"), 0o644))

	err := run(config{root: repoRoot(t), manifestPath: liveManifest(t), agentsDir: dir})
	require.Error(t, err)
	require.Contains(t, err.Error(), "surveyor")
}

// The inverse: the directive advertises an agent the clone does not ship, so
// the agent is told to dispatch something that is not installed.
func TestRun_FailsOnARosterNameThatIsNotInstalled(t *testing.T) {
	dir := liveAgentsDir(t)
	roster, err := bootstrap.DelegationRoster()
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(dir, roster[0]+".md")))

	err = run(config{root: repoRoot(t), manifestPath: liveManifest(t), agentsDir: dir})
	require.Error(t, err)
	require.Contains(t, err.Error(), roster[0])
}

// Fail-closed on an unusable oracle. Each of these is a fetch that produced
// something a warn-and-continue guard would read as a pass.
func TestRun_FailsClosedOnAnUnusableOracle(t *testing.T) {
	t.Run("manifest file absent", func(t *testing.T) {
		err := run(config{
			root:         repoRoot(t),
			manifestPath: filepath.Join(t.TempDir(), "nope.json"),
			agentsDir:    liveAgentsDir(t),
		})
		require.Error(t, err)
	})

	t.Run("manifest advertises no tools", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tool-manifest.json")
		require.NoError(t, os.WriteFile(path, []byte(`{"tools":[]}`), 0o644))
		err := run(config{root: repoRoot(t), manifestPath: path, agentsDir: liveAgentsDir(t)})
		require.Error(t, err)
	})

	t.Run("agents dir absent", func(t *testing.T) {
		err := run(config{
			root:         repoRoot(t),
			manifestPath: liveManifest(t),
			agentsDir:    filepath.Join(t.TempDir(), "nope"),
		})
		require.Error(t, err)
	})

	t.Run("agents dir empty", func(t *testing.T) {
		err := run(config{root: repoRoot(t), manifestPath: liveManifest(t), agentsDir: t.TempDir()})
		require.Error(t, err)
	})

	t.Run("root scans no prompt text", func(t *testing.T) {
		err := run(config{root: t.TempDir(), manifestPath: liveManifest(t), agentsDir: liveAgentsDir(t)})
		require.Error(t, err)
	})
}

// Both halves are reported in one run. A guard that returns on the first
// failure makes a two-problem tree take two CI round-trips to diagnose.
func TestRun_ReportsBothFailuresTogether(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool-manifest.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"tools":[{"name":"submit_outcome"}]}`), 0o644))
	dir := liveAgentsDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "surveyor.md"), []byte("# agent"), 0o644))

	err := run(config{root: repoRoot(t), manifestPath: path, agentsDir: dir})
	require.Error(t, err)
	require.Contains(t, err.Error(), "issue_write")
	require.Contains(t, err.Error(), "surveyor")
}
