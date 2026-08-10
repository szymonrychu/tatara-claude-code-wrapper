package bootstrap_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
)

// The agent runs headless. Bootstrap must always append a directive routing
// decisions to a LIVE MCP tool, appended after (not replacing) any provided
// GlobalClaudeMd.
//
// #136: this used to name comment_on_issue and decline_implementation, both
// reaped from tatara-cli on 2026-07-13. The two tools the directive may name
// are issue_write (the action="comment" form that replaced comment_on_issue)
// and submit_outcome (the always-on terminal tool that replaced
// decline_implementation). The retired names must never come back - a blocked
// agent following this text gets `tool not found` from the one instruction
// surface it reads when it is already stuck.
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
	require.Contains(t, got, `issue_write(action="comment"`)
	require.Contains(t, got, "submit_outcome")
	require.Contains(t, got, "AskUserQuestion")
	for _, reaped := range []string{"comment_on_issue", "decline_implementation", "already_done"} {
		require.NotContainsf(t, got, reaped,
			"the injected global CLAUDE.md names %q, retired from tatara-cli on 2026-07-13", reaped)
	}
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
	require.Contains(t, string(b), "issue_write")
}

// task-kind redesign Decision 6: the main agent should delegate work to the
// typed subagents (explorer/tester/builder/architect), keeping
// planning/design/review/merge on itself.
func TestRender_AppendsWorkerDelegationDirective(t *testing.T) {
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
	got := string(b)
	require.Contains(t, got, "explorer")
	require.Contains(t, got, "tester")
	require.Contains(t, got, "builder")
	require.Contains(t, got, "architect")
	require.Contains(t, got, "planning")
}

// The mid-turn push safety net auto-pushes the task branch, so a commit is
// public the moment it is made and "commit then amend" is no longer a
// supported workflow (force-push is in the settings deny list). The agent has
// to be told, or it will plan a rewrite it cannot publish.
func TestRender_AppendsAutoPushDirective(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()

	p := bootstrap.Params{
		HomeDir:     home,
		Workspace:   ws,
		HookCommand: "/usr/local/bin/cc-stop-hook",
		RepoURL:     "https://github.com/owner/repo",
		TaskBranch:  "tatara/feat-37-decks",
	}
	require.NoError(t, bootstrap.Render(p, func(dir string, a ...string) error { return nil }))

	b, err := os.ReadFile(filepath.Join(home, ".claude", "CLAUDE.md"))
	require.NoError(t, err)
	got := string(b)
	require.Contains(t, got, "pushed")
	require.Contains(t, got, "amend")
	require.Contains(t, got, "force-push")
	require.Contains(t, got, "make a new commit")
}

// A read-only review checkout (CheckoutBranch, no TaskBranch) never pushes, so
// telling that agent its branch is auto-pushed would be a plain lie.
func TestRender_NoAutoPushDirectiveWithoutTaskBranch(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()

	p := bootstrap.Params{
		HomeDir:        home,
		Workspace:      ws,
		HookCommand:    "/usr/local/bin/cc-stop-hook",
		RepoURL:        "https://github.com/owner/repo",
		CheckoutBranch: "pr-head",
	}
	require.NoError(t, bootstrap.Render(p, func(dir string, a ...string) error { return nil }))

	b, err := os.ReadFile(filepath.Join(home, ".claude", "CLAUDE.md"))
	require.NoError(t, err)
	require.NotContains(t, string(b), "force-push")
}
