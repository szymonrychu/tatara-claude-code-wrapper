package bootstrap

import (
	"encoding/json"
	"testing"
)

func TestMergeMCP_ExtraServersAdded(t *testing.T) {
	ws := t.TempDir()
	p := Params{Workspace: ws, BaseMCP: []byte(`{"mcpServers":{}}`),
		ExtraMCPServers: []byte(`[{"name":"spellslinger","url":"http://x:8080/mcp","type":"http"}]`)}
	if err := mergeMCP(p); err != nil {
		t.Fatal(err)
	}
	doc := readMCP(t, ws)
	entry, ok := doc.MCPServers["spellslinger"]
	if !ok {
		t.Fatal("spellslinger entry missing")
	}
	var got map[string]string
	_ = json.Unmarshal(entry, &got)
	if got["type"] != "http" || got["url"] != "http://x:8080/mcp" {
		t.Fatalf("bad entry: %v", got)
	}
}

func TestMergeMCP_ReservedNamesSkipped(t *testing.T) {
	ws := t.TempDir()
	p := Params{Workspace: ws, BaseMCP: []byte(`{"mcpServers":{}}`),
		GrafanaMCPURL:   "http://real-grafana/mcp",
		ExtraMCPServers: []byte(`[{"name":"grafana","url":"http://evil/mcp","type":"http"},{"name":"tatara","url":"http://evil2/mcp"},{"name":"serena","url":"http://evil3/mcp"}]`)}
	if err := mergeMCP(p); err != nil {
		t.Fatal(err)
	}
	doc := readMCP(t, ws)
	var g map[string]string
	_ = json.Unmarshal(doc.MCPServers["grafana"], &g)
	if g["url"] != "http://real-grafana/mcp" {
		t.Fatalf("reserved grafana overwritten by extra: %v", g)
	}
	if _, ok := doc.MCPServers["tatara"]; ok {
		t.Fatal("reserved tatara injected from extras")
	}
	if _, ok := doc.MCPServers["serena"]; ok {
		t.Fatal("reserved serena injected from extras")
	}
}

func TestMergeMCP_MalformedExtrasFailOpen(t *testing.T) {
	ws := t.TempDir()
	p := Params{Workspace: ws, BaseMCP: []byte(`{"mcpServers":{"base":{"type":"http","url":"http://b"}}}`),
		ExtraMCPServers: []byte(`not json`)}
	if err := mergeMCP(p); err != nil {
		t.Fatalf("malformed extras must not error: %v", err)
	}
	doc := readMCP(t, ws)
	if _, ok := doc.MCPServers["base"]; !ok {
		t.Fatal("base server lost on malformed extras")
	}
}

func TestMergeMCP_EntryMissingFieldsSkipped(t *testing.T) {
	ws := t.TempDir()
	p := Params{Workspace: ws, BaseMCP: []byte(`{"mcpServers":{}}`),
		ExtraMCPServers: []byte(`[{"name":"","url":"http://x"},{"name":"ok"}]`)}
	if err := mergeMCP(p); err != nil {
		t.Fatal(err)
	}
	doc := readMCP(t, ws)
	if len(doc.MCPServers) != 0 {
		t.Fatalf("invalid entries should be skipped, got %v", doc.MCPServers)
	}
}
