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

type claudeProc struct {
	cmd  *exec.Cmd
	ptmx *os.File
}

func spawnClaude(cfg Config) (*claudeProc, error) {
	args := cfg.claudeArgs()
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

func (c *claudeProc) Write(p []byte) (int, error) { return c.ptmx.Write(p) }
func (c *claudeProc) Close() error                { return c.ptmx.Close() }
