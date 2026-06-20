package bootstrap_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
)

// The agent runs headless with the issue thread as its only human channel.
// Bootstrap must always append a directive routing decisions to the
// comment_on_issue MCP tool, appended after (not replacing) any provided
// GlobalClaudeMd.
func TestRender_AppendsHeadlessDecisionDirective(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()

	p := bootstrap.Params{
		HomeDir:        home,
		Workspace:      ws,
		HookCommand:    "/usr/local/bin/cc-stop-hook",
		GlobalClaudeMd: "OPERATOR GLOBAL RULES",
	}
	require.NoError(t, bootstrap.Render(p, func(dir string, a ...string) error { return nil }))

	b, err := os.ReadFile(filepath.Join(home, ".claude", "CLAUDE.md"))
	require.NoError(t, err)
	got := string(b)

	require.Contains(t, got, "OPERATOR GLOBAL RULES")
	require.Contains(t, got, "comment_on_issue")
	require.Contains(t, got, "decline_implementation")
	require.Contains(t, got, "AskUserQuestion")
}

// The directive must be present even when no GlobalClaudeMd is configured.
func TestRender_DirectivePresentWithoutGlobalClaudeMd(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()

	p := bootstrap.Params{
		HomeDir:     home,
		Workspace:   ws,
		HookCommand: "/usr/local/bin/cc-stop-hook",
	}
	require.NoError(t, bootstrap.Render(p, func(dir string, a ...string) error { return nil }))

	b, err := os.ReadFile(filepath.Join(home, ".claude", "CLAUDE.md"))
	require.NoError(t, err)
	require.Contains(t, string(b), "comment_on_issue")
}
