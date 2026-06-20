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
	}
	// Always deny Claude's built-in interactive tools. Agent pods run headless
	// with no human at the terminal, so a picker can only stall the turn; the
	// issue thread is the real human channel. Deny wins even under
	// bypassPermissions, so these can never fire.
	perms := map[string]any{
		"deny": []string{"AskUserQuestion", "ExitPlanMode", "EnterPlanMode"},
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
