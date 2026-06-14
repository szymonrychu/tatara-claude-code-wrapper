package bootstrap

import (
	"log/slog"
	"path/filepath"
)

// InstallHooks runs `mise install` and `pre-commit install` in each cloned
// repo directory. It is best-effort: failures are logged but never abort
// the wrapper startup (a repo may lack .pre-commit-config.yaml or mise tasks).
//
// Directory resolution mirrors Render:
//   - When repos is non-empty: workspace/<namespacePath(r.URL)> for each entry.
//   - When repos is empty and repoURL is set: workspace itself.
func InstallHooks(workspace string, repos []RepoSpec, repoURL string, cmd CmdRunnerDir) {
	dirs := repoDirs(workspace, repos, repoURL)
	for _, dir := range dirs {
		if err := cmd(dir, "mise", "install"); err != nil {
			slog.Warn("mise install failed (best-effort)", "dir", dir, "error", err)
		}
		if err := cmd(dir, "pre-commit", "install", "--hook-type", "pre-commit", "--hook-type", "pre-push"); err != nil {
			slog.Warn("pre-commit install failed (best-effort)", "dir", dir, "error", err)
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
