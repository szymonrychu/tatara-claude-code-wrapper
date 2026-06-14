package session

import (
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// ptyWriter is the seam the Manager writes turns into. Real impl is a PTY
// master; tests substitute a fake.
type ptyWriter interface {
	io.Writer
	Close() error
}

// ClaudeProcess is the seam the Manager supervises. Real impl is *claudeProc
// over a PTY; tests substitute a fake whose Wait() they control.
// Exported so external test packages can implement the interface.
type ClaudeProcess interface {
	Wait() error
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Close() error
}

// claudeProcess is the internal alias.
type claudeProcess = ClaudeProcess

// ConfigClaudeArgs is an exported wrapper for Config.claudeArgs for use in
// external test packages.
func ConfigClaudeArgs(c Config, resume bool) []string { return c.claudeArgs(resume) }

type claudeProc struct {
	cmd  *exec.Cmd
	ptmx *os.File
}

func (c *claudeProc) Wait() error                 { return c.cmd.Wait() }
func (c *claudeProc) Read(p []byte) (int, error)  { return c.ptmx.Read(p) }
func (c *claudeProc) Write(p []byte) (int, error) { return c.ptmx.Write(p) }
func (c *claudeProc) Close() error                { return c.ptmx.Close() }

// claudeArgs builds the interactive launch flags. Per the Task-1 spike we pass
// NO permission flag: bypass is configured via settings.defaultMode
// (bootstrap). The "Bypass Permissions" warning dialog still appears at boot
// regardless and is accepted by bootWait. MCP comes from the cwd .mcp.json +
// enableAllProjectMcpServers, so no --mcp-config flag either.
func (c Config) claudeArgs(resume bool) []string {
	args := []string{}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	if resume {
		// Resume the most recent conversation in the workspace so a relaunched
		// session keeps its context after a crash.
		args = append(args, "--continue")
	}
	return args
}

func spawnClaude(cfg Config, resume bool) (*claudeProc, error) {
	args := cfg.claudeArgs(resume)
	cmd := exec.Command(cfg.ClaudePath, args...) //nolint:gosec // ClaudePath is operator-controlled config, not user input
	cmd.Dir = cfg.Workspace
	cmd.Env = cfg.Env
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("pty start claude: %w", err)
	}
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: 50, Cols: 200})
	return &claudeProc{cmd: cmd, ptmx: ptmx}, nil
}
