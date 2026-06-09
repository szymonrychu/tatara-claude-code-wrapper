package bootstrap

// CmdRunner runs an external command; injected for testability.
type CmdRunner func(name string, args ...string) error

// RegisterTataraMCP merges the tatara MCP server into the workspace .mcp.json
// via the tatara CLI's own mcp-config command.
func RegisterTataraMCP(workspace string, run CmdRunner) error {
	return run("tatara", "mcp-config", workspace)
}
