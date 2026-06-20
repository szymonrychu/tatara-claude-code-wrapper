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
	settings := map[string]any{}
	// Operator-provided extra settings go in FIRST (lowest priority): they fill
	// in any claude-code knob the operator does not manage itself (e.g.
	// maxParallelism), but must never clobber the hooks/effort/permissions the
	// operator owns, so every operator-managed key below overwrites them.
	if len(p.ExtraSettings) > 0 {
		var extra map[string]any
		if err := json.Unmarshal(p.ExtraSettings, &extra); err != nil {
			return fmt.Errorf("parse extra settings: %w", err)
		}
		for k, v := range extra {
			settings[k] = v
		}
	}
	settings["hooks"] = map[string]any{
		"Stop": []hookMatcher{{Matcher: "", Hooks: []hookCmd{{Type: "command", Command: p.HookCommand}}}},
	}
	settings["enableAllProjectMcpServers"] = p.EnableAllMCP
	if p.Effort != "" {
		settings["effortLevel"] = p.Effort
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
	// Declarative plugin install (operator-managed): the interactive /plugin
	// commands have no non-interactive flag, so plugins are enabled via
	// settings.json instead. Operator-managed, so these win over any same-named
	// keys an operator put in ExtraSettings.
	if markets, enabled := pluginConfig(p.Plugins); len(enabled) > 0 {
		if len(markets) > 0 {
			settings["extraKnownMarketplaces"] = markets
		}
		settings["enabledPlugins"] = enabled
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	if err := os.WriteFile(filepath.Join(claudeHome, "settings.json"), out, 0o644); err != nil {
		return fmt.Errorf("write settings.json: %w", err)
	}
	return nil
}
