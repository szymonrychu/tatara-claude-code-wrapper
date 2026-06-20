package bootstrap_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
)

// Headless agent pods have no human at the terminal, so Claude's built-in
// interactive tools (AskUserQuestion, ExitPlanMode) must always be denied,
// regardless of permission mode or allow-list config.
func TestRender_AlwaysDeniesInteractiveTools(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()

	p := bootstrap.Params{
		HomeDir:     home,
		Workspace:   ws,
		HookCommand: "/usr/local/bin/cc-stop-hook",
		// no PermissionMode, no AllowedTools: deny must still be emitted
	}
	require.NoError(t, bootstrap.Render(p, func(dir string, a ...string) error { return nil }))

	b, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	require.NoError(t, err)

	var s struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	require.NoError(t, json.Unmarshal(b, &s))
	require.Contains(t, s.Permissions.Deny, "AskUserQuestion")
	require.Contains(t, s.Permissions.Deny, "ExitPlanMode")
	require.Contains(t, s.Permissions.Deny, "EnterPlanMode")
}
