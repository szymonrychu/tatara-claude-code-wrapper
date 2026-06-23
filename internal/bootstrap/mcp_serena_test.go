package bootstrap

import (
	"encoding/json"
	"testing"
)

func TestMergeMCP_AddsSerenaWhenSet(t *testing.T) {
	ws := t.TempDir()
	err := mergeMCP(Params{
		Workspace:    ws,
		BaseMCP:      []byte(`{"mcpServers":{"tatara":{"command":"tatara","args":["mcp"]}}}`),
		SerenaMCPURL: "http://serena.tatara.svc:9121/mcp",
	})
	if err != nil {
		t.Fatalf("mergeMCP: %v", err)
	}
	d := readMCP(t, ws)
	if _, ok := d.MCPServers["tatara"]; !ok {
		t.Fatalf("tatara entry must be preserved")
	}
	s, ok := d.MCPServers["serena"]
	if !ok {
		t.Fatalf("serena entry missing")
	}
	var entry map[string]string
	_ = json.Unmarshal(s, &entry)
	if entry["type"] != "http" || entry["url"] != "http://serena.tatara.svc:9121/mcp" {
		t.Fatalf("serena entry wrong: %s", string(s))
	}
}

func TestMergeMCP_NoSerenaWhenUnset(t *testing.T) {
	ws := t.TempDir()
	if err := mergeMCP(Params{Workspace: ws, BaseMCP: []byte(`{"mcpServers":{}}`)}); err != nil {
		t.Fatalf("mergeMCP: %v", err)
	}
	if _, ok := readMCP(t, ws).MCPServers["serena"]; ok {
		t.Fatalf("serena entry must be absent when URL unset")
	}
}
