package bootstrap

import (
	"encoding/json"
	"fmt"
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
	if p.GrafanaMCPURL != "" {
		entry, _ := json.Marshal(map[string]string{"type": "http", "url": p.GrafanaMCPURL})
		merged.MCPServers["grafana"] = entry
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
