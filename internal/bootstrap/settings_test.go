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

// Deterministic guardrails: `deny` wins even under bypassPermissions, so the
// platform's hard rules (deploy ONLY via tatara-helmfile GitOps; never leak
// secrets/state) are enforced by the client regardless of what the agent
// decides. These must always be present.
func TestRender_DeniesDeployAndSecretAccess(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()

	p := bootstrap.Params{
		HomeDir:     home,
		Workspace:   ws,
		HookCommand: "/usr/local/bin/cc-stop-hook",
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
	// hand-deploy verbs (rule: deploy only through tatara-helmfile GitOps)
	require.Contains(t, s.Permissions.Deny, "Bash(kubectl patch *)")
	require.Contains(t, s.Permissions.Deny, "Bash(kubectl set *)")
	require.Contains(t, s.Permissions.Deny, "Bash(helm upgrade *)")
	// history rewrite + secret/state exfil
	require.Contains(t, s.Permissions.Deny, "Bash(git push --force*)")
	require.Contains(t, s.Permissions.Deny, "Read(**/*.tfstate)")
	require.Contains(t, s.Permissions.Deny, "Read(**/secrets/**)")
}
