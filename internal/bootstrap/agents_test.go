package bootstrap

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
)

func TestInstallAgents_CopiesTopLevelMDFiles(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "explorer.md"), "---\nname: explorer\nmodel: haiku\n---\n# explorer")
	mustWriteFile(t, filepath.Join(src, "builder.md"), "---\nname: builder\nmodel: sonnet\n---\n# builder")

	ws := t.TempDir()
	err := installAgents(Params{Workspace: ws, AgentsSrc: []string{src}})
	require.NoError(t, err)

	dst := filepath.Join(ws, ".claude", "agents")
	require.FileExists(t, filepath.Join(dst, "explorer.md"))
	require.FileExists(t, filepath.Join(dst, "builder.md"))
}

func TestInstallAgents_IgnoresNonMDAndSubdirs(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "explorer.md"), "# explorer")
	mustWriteFile(t, filepath.Join(src, "README"), "not an agent")
	mustMkdir(t, filepath.Join(src, "nested"))
	mustWriteFile(t, filepath.Join(src, "nested", "ignored.md"), "# ignored, not top-level")

	ws := t.TempDir()
	require.NoError(t, installAgents(Params{Workspace: ws, AgentsSrc: []string{src}}))

	dst := filepath.Join(ws, ".claude", "agents")
	require.FileExists(t, filepath.Join(dst, "explorer.md"))
	require.NoFileExists(t, filepath.Join(dst, "README"))
	require.NoFileExists(t, filepath.Join(dst, "ignored.md"))
}

func TestInstallAgents_LaterSourceWins(t *testing.T) {
	src1 := t.TempDir()
	src2 := t.TempDir()
	mustWriteFile(t, filepath.Join(src1, "explorer.md"), "from src1")
	mustWriteFile(t, filepath.Join(src2, "explorer.md"), "from src2")

	ws := t.TempDir()
	require.NoError(t, installAgents(Params{Workspace: ws, AgentsSrc: []string{src1, src2}}))

	got, err := os.ReadFile(filepath.Join(ws, ".claude", "agents", "explorer.md"))
	require.NoError(t, err)
	require.Equal(t, "from src2", string(got))
}

func TestInstallAgents_MissingSrcDir_NoOp(t *testing.T) {
	ws := t.TempDir()
	err := installAgents(Params{Workspace: ws, AgentsSrc: []string{filepath.Join(ws, "does-not-exist")}})
	require.NoError(t, err)
	require.NoDirExists(t, filepath.Join(ws, ".claude", "agents", "does-not-exist"))
}

func TestInstallAgents_EmptyAgentsSrc_CreatesEmptyAgentsDir(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, installAgents(Params{Workspace: ws}))
	require.DirExists(t, filepath.Join(ws, ".claude", "agents"))
}

func TestInstallAgents_PreservesExecutableBit(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "explorer.md"), "# explorer")
	// agent files are plain markdown, not scripts, but the copy helper is
	// shared with installSkills - lock in that permissions still round-trip.
	require.NoError(t, os.Chmod(filepath.Join(src, "explorer.md"), 0o644)) //nolint:gosec // test fixture, not a secret file

	ws := t.TempDir()
	require.NoError(t, installAgents(Params{Workspace: ws, AgentsSrc: []string{src}}))

	info, err := os.Stat(filepath.Join(ws, ".claude", "agents", "explorer.md"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), info.Mode().Perm())
}

func TestInstallAgents_MetricCounted(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "explorer.md"), "# explorer")
	mustWriteFile(t, filepath.Join(src, "builder.md"), "# builder")

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	ws := t.TempDir()
	require.NoError(t, installAgents(Params{Workspace: ws, AgentsSrc: []string{src}, M: m}))

	mf, err := reg.Gather()
	require.NoError(t, err)
	var total float64
	for _, fam := range mf {
		if fam.GetName() == "wrapper_agents_installed_total" {
			for _, mm := range fam.GetMetric() {
				total += mm.GetCounter().GetValue()
			}
		}
	}
	require.Equal(t, float64(2), total, "installed counter must count both agent files")
}

func TestInstallAgents_LogsShadowedAgent(t *testing.T) {
	src1 := t.TempDir()
	src2 := t.TempDir()
	mustWriteFile(t, filepath.Join(src1, "explorer.md"), "from src1")
	mustWriteFile(t, filepath.Join(src2, "explorer.md"), "from src2")

	ws := t.TempDir()
	var logBuf strings.Builder
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	require.NoError(t, installAgents(Params{Workspace: ws, AgentsSrc: []string{src1, src2}, Log: log}))
	require.Contains(t, logBuf.String(), "explorer.md")
}
