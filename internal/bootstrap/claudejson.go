package bootstrap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// writeClaudeJSON seeds ~/.claude.json so a fresh-HOME unattended claude boots
// with no interactive dialogs (onboarding, folder trust, custom-API-key).
// Recipe confirmed by the Task-1 spike (docs/spike-findings.md).
func writeClaudeJSON(p Params) error {
	doc := map[string]any{
		"hasCompletedOnboarding": true,
		"autoUpdates":            true,
		"projects": map[string]any{
			p.Workspace: map[string]any{"hasTrustDialogAccepted": true},
		},
	}
	if p.AnthropicAPIKey != "" {
		doc["customApiKeyResponses"] = map[string]any{
			"approved": []string{lastN(p.AnthropicAPIKey, 20)},
			"rejected": []string{},
		}
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal claude.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(p.HomeDir, ".claude.json"), b, 0o644); err != nil {
		return fmt.Errorf("write claude.json: %w", err)
	}
	return nil
}

func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// WriteClaudeJSONForTest exposes writeClaudeJSON for the package test.
func WriteClaudeJSONForTest(p Params) error { return writeClaudeJSON(p) }
