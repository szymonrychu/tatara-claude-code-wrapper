package bootstrap_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
)

// delegationRosterSnapshot is the exhaustive set of typed subagent names the
// injected global CLAUDE.md is allowed to name. It is a trip-wire, not
// documentation - the same species as promptguard_test.go's
// promptToolNamesSnapshot, and it exists for the same reason: a hardcoded name
// in agent-facing prompt text outliving its source of truth.
//
// promptguard cannot see this class. Its extractor requires an underscore
// (internal/promptguard/promptguard.go's toolNameToken), so it walks the
// delegation directive, extracts nothing, and reports clean. It named four
// agents while tatara-agent-skills shipped five for as long as `writer`
// existed.
//
// The LIVE half is .github/ci/check-agent-facing-names.sh, which reconciles
// this roster against the .claude/agents listing in the skills tarball at the
// pinned TATARA_SKILLS_REF. This snapshot is the offline half: it fails here,
// on this repo's own text, without a network.
var delegationRosterSnapshot = []string{"architect", "builder", "explorer", "tester", "writer"}

func TestDelegationRoster_MatchesSnapshot(t *testing.T) {
	got, err := bootstrap.DelegationRoster()
	require.NoError(t, err)
	require.Equal(t, delegationRosterSnapshot, got,
		"the delegation directive names a different set of typed subagents than the snapshot. "+
			"installAgents copies EVERY top-level *.md from the skills clone, so a name missing "+
			"here is an agent that lands in the pod and is never advertised to the agent that "+
			"could dispatch it.")
}

// DelegationRoster must fail closed. A parser that silently returns nothing
// makes the guard report "roster matches" against an empty set.
func TestDelegationRoster_IsNotEmpty(t *testing.T) {
	got, err := bootstrap.DelegationRoster()
	require.NoError(t, err)
	require.NotEmpty(t, got)
}

func TestAgentNames_ReadsTopLevelMarkdownBasenames(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"architect.md", "writer.md", "explorer.md"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte("# agent"), 0o644))
	}
	// Neither of these is an agent definition: installAgentsFromSrc skips
	// subdirectories and non-.md files, and AgentNames must share that glob
	// rather than re-implement it.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "nested"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.txt"), []byte("x"), 0o644))

	got, err := bootstrap.AgentNames(dir)
	require.NoError(t, err)
	require.Equal(t, []string{"architect", "explorer", "writer"}, got)
}

// An absent or empty agents directory is exit-1 material for the guard, not a
// clean "roster matches nothing". This is the fail-open shape
// promptguard.Literals already refuses and validate_tool_calls.py (one repo
// over) still has.
func TestAgentNames_EmptyDirIsAnError(t *testing.T) {
	_, err := bootstrap.AgentNames(t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "fail open")
}

func TestAgentNames_MissingDirIsAnError(t *testing.T) {
	_, err := bootstrap.AgentNames(filepath.Join(t.TempDir(), "does-not-exist"))
	require.Error(t, err)
}
