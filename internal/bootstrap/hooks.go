package bootstrap

import (
	"log/slog"
	"path/filepath"
	"time"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
)

// InstallHooks runs `mise install` and `pre-commit install` in each cloned
// repo directory. It is best-effort: failures are logged but never abort
// the wrapper startup (a repo may lack .pre-commit-config.yaml or mise tasks).
//
// Directory resolution mirrors Render:
//   - When repos is non-empty: workspace/<namespacePath(r.URL)> for each entry.
//   - When repos is empty and repoURL is set: workspace itself.
func InstallHooks(workspace string, repos []RepoSpec, repoURL string, cmd CmdRunnerDir, log *slog.Logger, m *metrics.Metrics) {
	dirs := repoDirs(workspace, repos, repoURL)
	for _, dir := range dirs {
		runHookInstall(dir, "mise", []string{"install"}, cmd, log, m)
		runHookInstall(dir, "pre-commit", []string{"install", "--hook-type", "pre-commit", "--hook-type", "pre-push"}, cmd, log, m)
	}
}

func runHookInstall(dir, tool string, args []string, cmd CmdRunnerDir, log *slog.Logger, m *metrics.Metrics) {
	start := time.Now()
	err := cmd(dir, tool, args...)
	result := "ok"
	if err != nil {
		result = "fail"
	}
	if m != nil {
		m.BootstrapHookInstall.WithLabelValues(result, tool).Inc()
	}
	if log != nil {
		if err != nil {
			log.Warn("hook install failed (best-effort)", "action", "hook_install", "tool", tool, "dir", dir, "error", err, "duration_ms", time.Since(start).Milliseconds())
		} else {
			log.Info("hook install ok", "action", "hook_install", "tool", tool, "dir", dir, "duration_ms", time.Since(start).Milliseconds())
		}
	}
}

// repoDirs returns the list of repo directories that InstallHooks should
// operate on, using the same path resolution as Render.
func repoDirs(workspace string, repos []RepoSpec, repoURL string) []string {
	if len(repos) > 0 {
		var dirs []string
		for _, r := range repos {
			ns := namespacePath(r.URL)
			if ns == "" || filepath.Clean(filepath.Join(workspace, ns)) == filepath.Clean(workspace) {
				continue
			}
			dirs = append(dirs, filepath.Join(workspace, ns))
		}
		return dirs
	}
	if repoURL != "" {
		return []string{workspace}
	}
	return nil
}
