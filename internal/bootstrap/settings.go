package bootstrap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func writeSettings(p Params, claudeHome string) error {
	type hookCmd struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	type hookMatcher struct {
		Matcher string    `json:"matcher"`
		Hooks   []hookCmd `json:"hooks"`
	}
	settings := map[string]any{
		"hooks": map[string]any{
			"Stop": []hookMatcher{{Matcher: "", Hooks: []hookCmd{{Type: "command", Command: p.HookCommand}}}},
		},
		"enableAllProjectMcpServers": p.EnableAllMCP,
		"attribution": map[string]any{
			"commit":     "",
			"pr":         "",
			"sessionUrl": false,
		},
	}
	if p.Effort != "" {
		settings["effortLevel"] = p.Effort
	}
	// Deny wins even under bypassPermissions, so this is the hard-enforcement
	// layer independent of the agent's judgment. Three groups:
	//  1. Interactive tools: agent pods run headless with no human at the
	//     terminal, so a picker can only stall the turn; the issue thread is the
	//     real human channel.
	//  2. Hand-deploy verbs: the platform deploys ONLY through tatara-helmfile
	//     GitOps. `kubectl set|patch|edit` and `helm upgrade|install|uninstall|
	//     rollback` to ship an image/chart are forbidden (rule 15); read verbs
	//     (get/describe/logs) and `helm template|lint` stay allowed.
	//  3. Exfil / irreversible: never leak tfstate or sops secrets, never
	//     rewrite history with a force-push, never `rm -rf /`.
	deny := []string{
		"AskUserQuestion", "ExitPlanMode", "EnterPlanMode",
		"Bash(kubectl set *)", "Bash(kubectl patch *)", "Bash(kubectl edit *)",
		"Bash(helm upgrade *)", "Bash(helm install *)", "Bash(helm uninstall *)", "Bash(helm rollback *)",
		"Bash(git push --force*)", "Bash(git push -f *)", "Bash(rm -rf /*)",
		"Read(**/*.tfstate)", "Read(**/secrets/**)",
		"Read(**/*.secret.*.yaml)", "Read(**/*.secret.*.yml)", "Read(**/default.secrets.yaml)",
	}
	perms := map[string]any{
		"deny": deny,
	}
	if p.PermissionMode != "" {
		perms["defaultMode"] = p.PermissionMode
	}
	if len(p.AllowedTools) > 0 {
		perms["allow"] = p.AllowedTools
	}
	settings["permissions"] = perms
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	if err := os.WriteFile(filepath.Join(claudeHome, "settings.json"), out, 0o644); err != nil {
		return fmt.Errorf("write settings.json: %w", err)
	}
	return nil
}
