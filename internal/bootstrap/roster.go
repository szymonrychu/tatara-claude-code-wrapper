package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// rosterBullet matches one typed-subagent bullet in delegationDirective:
// `- **explorer**: read-only code search, ...`. The name is the bolded token.
var rosterBullet = regexp.MustCompile(`(?m)^- \*\*([a-z][a-z0-9-]*)\*\*:`)

// DelegationRoster returns the sorted, de-duplicated typed-subagent names the
// injected global CLAUDE.md advertises via delegationDirective.
//
// It exists so a guard can reconcile that hardcoded roster against the set
// installAgents actually copies out of the skills clone. promptguard cannot do
// it: its extractor requires an underscore, so it walks this very literal and
// extracts nothing (see internal/promptguard/promptguard.go's toolNameToken).
//
// Fails closed on an empty parse, for the reason Literals does: a parser that
// matches nothing reports "the roster is complete" exactly the way a broken
// grep does.
func DelegationRoster() ([]string, error) {
	ms := rosterBullet.FindAllStringSubmatch(delegationDirective, -1)
	seen := map[string]bool{}
	for _, m := range ms {
		seen[m[1]] = true
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("bootstrap: delegationDirective lists no typed subagents; the roster parse matched nothing and would fail open")
	}
	return sortedKeys(seen), nil
}

// AgentNames returns the sorted names of the typed subagent definitions in dir,
// i.e. the basenames (without .md) of exactly the files installAgentsFromSrc
// would copy. Both go through agentFiles so the guard and the installer share
// one glob instead of drifting apart.
//
// An unreadable or empty directory is an error: a guard fed an agents dir that
// silently contains nothing would report the roster complete against the empty
// set.
func AgentNames(dir string) ([]string, error) {
	entries, err := agentFiles(dir)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[strings.TrimSuffix(e.Name(), ".md")] = true
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("bootstrap: no agent definitions in %s; the listing matched nothing and would fail open", dir)
	}
	return sortedKeys(seen), nil
}

// agentFiles is the single definition of "what counts as an agent definition":
// a top-level *.md file. Agent definitions ship flat at the plugin's
// .claude/agents/ root, so this deliberately does not recurse.
func agentFiles(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", filepath.Clean(dir), err)
	}
	out := make([]os.DirEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
