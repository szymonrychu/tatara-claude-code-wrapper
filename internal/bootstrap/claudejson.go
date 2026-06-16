package bootstrap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// writeClaudeJSON seeds ~/.claude.json so a fresh-HOME unattended claude boots
// with no interactive dialogs (onboarding, folder trust, custom-API-key).
// When the file already exists (persistent HOME or pod restart) the required
// keys are merged in rather than the file being overwritten wholesale, so any
// claude-written state from a prior run is preserved.
// Recipe confirmed by the Task-1 spike (docs/spike-findings.md).
func writeClaudeJSON(p Params) error {
	path := filepath.Join(p.HomeDir, ".claude.json")

	// Start from an empty doc or from the existing file if present.
	doc := map[string]any{}
	if existing, readErr := os.ReadFile(path); readErr == nil {
		// Ignore unmarshal errors: a corrupt file is treated as absent.
		_ = json.Unmarshal(existing, &doc)
	}

	// Inject / override the required dialog-suppression keys.
	doc["hasCompletedOnboarding"] = true
	doc["autoUpdates"] = true

	// Ensure the workspace project entry has hasTrustDialogAccepted.
	projects, _ := doc["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
	}
	proj, _ := projects[p.Workspace].(map[string]any)
	if proj == nil {
		proj = map[string]any{}
	}
	proj["hasTrustDialogAccepted"] = true
	projects[p.Workspace] = proj
	doc["projects"] = projects

	if p.AnthropicAPIKey != "" {
		suffix := lastN(p.AnthropicAPIKey, 20)
		// Merge approved list: add the suffix if not already present.
		responses, _ := doc["customApiKeyResponses"].(map[string]any)
		if responses == nil {
			responses = map[string]any{}
		}
		approved := toStringSlice(responses["approved"])
		if !contains(approved, suffix) {
			approved = append(approved, suffix)
		}
		if responses["rejected"] == nil {
			responses["rejected"] = []string{}
		}
		responses["approved"] = approved
		doc["customApiKeyResponses"] = responses
	}

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal claude.json: %w", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("write claude.json: %w", err)
	}
	return nil
}

func toStringSlice(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// WriteClaudeJSONForTest exposes writeClaudeJSON for the package test.
func WriteClaudeJSONForTest(p Params) error { return writeClaudeJSON(p) }
