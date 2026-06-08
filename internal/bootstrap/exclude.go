package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
)

// excludeWorkspaceConfig adds the wrapper's injected session files to the repo's
// local git exclude so `git add -A` never commits them into the agent's branch.
// These (.mcp.json, .claude/) are agent runtime config, not part of the repo.
func excludeWorkspaceConfig(workspace string) error {
	dir := filepath.Join(workspace, ".git", "info")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir git info: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "exclude"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open git exclude: %w", err)
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString("\n# tatara wrapper session config (not part of the repo)\n.mcp.json\n.claude/\n")
	return err
}
