package bootstrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func readMCP(t *testing.T, ws string) mcpDoc {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(ws, ".mcp.json"))
	if err != nil {
		t.Fatalf("read .mcp.json: %v", err)
	}
	var d mcpDoc
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return d
}

func TestMergeMCP_AddsGrafanaWhenSet(t *testing.T) {
	ws := t.TempDir()
	err := mergeMCP(Params{
		Workspace:     ws,
		BaseMCP:       []byte(`{"mcpServers":{"tatara":{"command":"tatara","args":["mcp"]}}}`),
		GrafanaMCPURL: "http://grafana-mcp-acme.tatara.svc:8000/mcp",
	})
	if err != nil {
		t.Fatalf("mergeMCP: %v", err)
	}
	d := readMCP(t, ws)
	if _, ok := d.MCPServers["tatara"]; !ok {
		t.Fatalf("tatara entry must be preserved")
	}
	g, ok := d.MCPServers["grafana"]
	if !ok {
		t.Fatalf("grafana entry missing")
	}
	var entry map[string]string
	_ = json.Unmarshal(g, &entry)
	if entry["type"] != "http" || entry["url"] != "http://grafana-mcp-acme.tatara.svc:8000/mcp" {
		t.Fatalf("grafana entry wrong: %s", string(g))
	}
}

func TestMergeMCP_NoGrafanaWhenUnset(t *testing.T) {
	ws := t.TempDir()
	if err := mergeMCP(Params{Workspace: ws, BaseMCP: []byte(`{"mcpServers":{}}`)}); err != nil {
		t.Fatalf("mergeMCP: %v", err)
	}
	if _, ok := readMCP(t, ws).MCPServers["grafana"]; ok {
		t.Fatalf("grafana entry must be absent when URL unset")
	}
}
