package bootstrap

// CmdRunner runs an external command; injected for testability.
type CmdRunner func(name string, args ...string) error

// CmdRunnerDir runs an external command in a specific directory; injected for testability.
type CmdRunnerDir func(dir, name string, args ...string) error

// RegisterTataraMCP wires the tatara MCP server into the workspace .mcp.json
// via the tatara CLI's own mcp-config command. This writes a single entry
// {"command":"tatara","args":["mcp"]}; the set of MCP tools agents see is
// whatever `tatara mcp` serves (OperatorTools()), NOT enumerated here. New
// operator tools therefore flow through automatically once the baked tatara
// binary is rebuilt; mcp_flowthrough_test.go guards that against regression.
func RegisterTataraMCP(workspace string, run CmdRunner) error {
	return run("tatara", "mcp-config", workspace)
}
