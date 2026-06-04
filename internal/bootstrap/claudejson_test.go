package bootstrap_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
)

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
