package bootstrap_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
)

// TestWriteClaudeJSON_MergesExistingFile verifies that writeClaudeJSON preserves
// keys already present in ~/.claude.json rather than overwriting them wholesale
// (finding 5: read-merge, not clobber).
func TestWriteClaudeJSON_MergesExistingFile(t *testing.T) {
	home := t.TempDir()
	existing := map[string]any{
		"someOtherKey":           "preserved",
		"hasCompletedOnboarding": false, // will be overridden to true
	}
	b, _ := json.Marshal(existing)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	p := bootstrap.Params{HomeDir: home, Workspace: "/workspace",
		AnthropicAPIKey: "sk-ant-XXXXXXXXXXXXXXXXXXXXEentiTPHC9Q-62Rz1wAA"}
	require.NoError(t, bootstrap.WriteClaudeJSONForTest(p))

	b2, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	var doc map[string]any
	require.NoError(t, json.Unmarshal(b2, &doc))

	// Required dialog-suppression keys must be set.
	require.Equal(t, true, doc["hasCompletedOnboarding"])
	// Pre-existing key must survive.
	require.Equal(t, "preserved", doc["someOtherKey"],
		"writeClaudeJSON must preserve pre-existing keys, not overwrite them")
}

func TestWriteClaudeJSON_SeedsNoDialogKeys(t *testing.T) {
	home := t.TempDir()
	p := bootstrap.Params{HomeDir: home, Workspace: "/workspace",
		AnthropicAPIKey: "sk-ant-XXXXXXXXXXXXXXXXXXXXEentiTPHC9Q-62Rz1wAA"}
	require.NoError(t, bootstrap.WriteClaudeJSONForTest(p))
	b, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	var doc map[string]any
	require.NoError(t, json.Unmarshal(b, &doc))
	require.Equal(t, true, doc["hasCompletedOnboarding"])
	approved := doc["customApiKeyResponses"].(map[string]any)["approved"].([]any)
	require.Equal(t, "EentiTPHC9Q-62Rz1wAA", approved[0]) // last 20 chars
	proj := doc["projects"].(map[string]any)["/workspace"].(map[string]any)
	require.Equal(t, true, proj["hasTrustDialogAccepted"])
}
