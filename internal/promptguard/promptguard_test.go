package promptguard_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/promptguard"
)

// scan is the repo-wide prompt scan every test here works from.
func scan(t *testing.T) []promptguard.Literal {
	t.Helper()
	root, err := promptguard.RepoRoot()
	require.NoError(t, err)
	lits, err := promptguard.Literals(root)
	require.NoError(t, err)
	return lits
}

// The scan must actually reach the injected-CLAUDE.md directives. If it stops
// finding them (moved to a template file, split below the length floor, a
// walk that silently skips the package) every other assertion here becomes
// vacuous, so this is checked explicitly rather than assumed.
func TestLiterals_CoversTheInjectedClaudeMdDirectives(t *testing.T) {
	lits := scan(t)
	var found bool
	for _, l := range lits {
		if strings.Contains(l.File, "internal/bootstrap/bootstrap.go") &&
			strings.Contains(l.Text, "Headless agent: no interactive prompts") {
			found = true
		}
	}
	require.True(t, found,
		"the prompt scan no longer reaches headlessDirective in internal/bootstrap/bootstrap.go; "+
			"the guard is inert until the scan is taught where the prompt text moved. Literals found: %d", len(lits))
}

// PromptToolNamesSnapshot is the exhaustive set of MCP tool names this repo's
// agent-facing prompt text is allowed to reference. It is a trip-wire, not
// documentation: adding a tool name to any prompt block fails this test until
// the name is added here, which is the moment to check the name is real.
//
// TestAgentPromptToolNamesAreAdvertised (internal/bootstrap, runs in the
// Dockerfile guard stage against the baked cli) then proves every name in the
// scan is still advertised by the live MCP server.
//
// Why these two:
//   - issue_write is the surviving replacement for the reaped comment_on_issue
//     (tatara-agent-skills skills/mcp/tatara-mcp-scm/SKILL.md documents the
//     action="comment" form).
//   - submit_outcome is the one always-on terminal tool every profile has, and
//     replaced the reaped decline_implementation.
var promptToolNamesSnapshot = []string{"issue_write", "submit_outcome"}

func TestPromptToolNames_MatchSnapshot(t *testing.T) {
	lits := scan(t)
	got := promptguard.ToolNames(lits)
	require.Equal(t, promptToolNamesSnapshot, got,
		"agent-facing prompt text references a different set of MCP tool names than the snapshot. "+
			"If a name was added, confirm the live MCP server advertises it and update promptToolNamesSnapshot; "+
			"if a name was removed, drop it here too. Never widen this by adding a name you have not verified.")
}

// The extractor must not fail open on the exact names that caused #136: they
// are ordinary snake_case identifiers and must be reported wherever they appear
// in prompt text.
func TestToolNamesIn_ExtractsReapedNamesRatherThanSkippingThem(t *testing.T) {
	got := promptguard.ToolNamesIn("call comment_on_issue, or decline_implementation when blocked")
	require.Equal(t, []string{"comment_on_issue", "decline_implementation"}, got)
}

// Claude's built-in interactive tools are named in the same directive and are
// not MCP tools; they have no tools/list entry, so matching them would make the
// live guard permanently red.
func TestToolNamesIn_IgnoresClaudeBuiltins(t *testing.T) {
	require.Empty(t, promptguard.ToolNamesIn("AskUserQuestion, ExitPlanMode and EnterPlanMode are disabled"))
}

// Literals must report an empty scan as an error rather than returning a clean
// nil, which a caller would read as "no stale names".
func TestLiterals_EmptyScanIsAnError(t *testing.T) {
	_, err := promptguard.Literals(t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "fail open")
}
