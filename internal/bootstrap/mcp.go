package bootstrap

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

type mcpDoc struct {
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
}

// mergeMCP unions the baked base config with every *.json fragment in the
// overlay dir and writes /workspace/.mcp.json. Overlay keys win on conflict.
func mergeMCP(p Params) error {
	merged := mcpDoc{MCPServers: map[string]json.RawMessage{}}
	if len(p.BaseMCP) > 0 {
		var base mcpDoc
		if err := json.Unmarshal(p.BaseMCP, &base); err != nil {
			return fmt.Errorf("parse base mcp: %w", err)
		}
		for k, v := range base.MCPServers {
			merged.MCPServers[k] = v
		}
	}
	if p.MCPOverlayDir != "" {
		entries, err := os.ReadDir(p.MCPOverlayDir)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("read mcp overlay: %w", err)
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
				continue
			}
			b, err := os.ReadFile(filepath.Join(p.MCPOverlayDir, e.Name()))
			if err != nil {
				return fmt.Errorf("read overlay %s: %w", e.Name(), err)
			}
			var frag mcpDoc
			if err := json.Unmarshal(b, &frag); err != nil {
				return fmt.Errorf("parse overlay %s: %w", e.Name(), err)
			}
			for k, v := range frag.MCPServers {
				merged.MCPServers[k] = v
			}
		}
	}
	if len(p.ExtraMCPServers) > 0 {
		reserved := map[string]bool{"tatara": true, "grafana": true, "serena": true}
		var extras []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(p.ExtraMCPServers, &extras); err != nil {
			slog.Warn("mergeMCP: TATARA_EXTRA_MCP_SERVERS is not valid JSON, ignoring", "error", err)
		} else {
			for _, e := range extras {
				if e.Name == "" || e.URL == "" {
					slog.Warn("mergeMCP: skipping extra MCP server with empty name or url", "name", e.Name)
					continue
				}
				if reserved[e.Name] {
					slog.Warn("mergeMCP: skipping extra MCP server with reserved name", "name", e.Name)
					continue
				}
				typ := e.Type
				if typ == "" {
					typ = "http"
				}
				entry, _ := json.Marshal(map[string]string{"type": typ, "url": e.URL})
				merged.MCPServers[e.Name] = entry
			}
		}
	}
	if p.GrafanaMCPURL != "" {
		entry, _ := json.Marshal(map[string]string{"type": "http", "url": p.GrafanaMCPURL})
		merged.MCPServers["grafana"] = entry
	}
	if p.SerenaMCPURL != "" {
		entry, _ := json.Marshal(map[string]string{"type": "http", "url": p.SerenaMCPURL})
		merged.MCPServers["serena"] = entry
	}
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal mcp: %w", err)
	}
	if err := os.WriteFile(filepath.Join(p.Workspace, ".mcp.json"), out, 0o644); err != nil {
		return fmt.Errorf("write .mcp.json: %w", err)
	}
	return nil
}
