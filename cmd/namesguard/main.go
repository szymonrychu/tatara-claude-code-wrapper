// Command namesguard reconciles the hardcoded names this repo puts in
// agent-facing prompt text against the two upstream rosters that actually
// define them, offline, from oracles a pull_request can fetch without a
// credential:
//
//   - every MCP tool name in the injected global CLAUDE.md must appear in
//     tatara-cli's tool-manifest.json at the PINNED TATARA_CLI_VERSION, and
//   - the typed-subagent roster delegationDirective advertises must equal the
//     set installAgents copies out of tatara-agent-skills' .claude/agents at
//     the PINNED TATARA_SKILLS_REF.
//
// Why a command and not a test: the equivalent live checks
// (TestAgentPromptToolNamesAreAdvertised, TestTataraMCP_AdvertisesScmProjectTools)
// SKIP when no tatara binary is baked, which on the pull_request path is
// always - they are green by skip on every PR and only run at release, in the
// Dockerfile test-guard stage, which is outside the runtime DAG and is reached
// only by build.sh's explicit --opt target=test-guard. A binary with no skip
// semantics cannot go green by running nothing. #193.
//
// This is STRICTLY WEAKER than the stage it compensates for and does not
// replace it. The manifest carries names and enums and no profile data: a tool
// granted to no profile passes here and fails the live oracle. The stage stays.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/promptguard"
)

type config struct {
	root         string
	manifestPath string
	agentsDir    string
}

func main() {
	var cfg config
	flag.StringVar(&cfg.root, "root", ".", "repo root to scan for agent-facing prompt text")
	flag.StringVar(&cfg.manifestPath, "tool-manifest", "", "path to tatara-cli's tool-manifest.json at the pinned version")
	flag.StringVar(&cfg.agentsDir, "agents-dir", "", "path to the skills plugin's .claude/agents at the pinned ref")
	flag.Parse()

	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("agent-facing names guard: ok")
}

// run reports EVERY reconciliation failure it finds rather than the first, so a
// tree that has both problems is diagnosable in one CI round-trip.
func run(cfg config) error {
	var problems []error
	if err := checkToolNames(cfg); err != nil {
		problems = append(problems, err)
	}
	if err := checkRoster(cfg); err != nil {
		problems = append(problems, err)
	}
	return errors.Join(problems...)
}

// checkToolNames is the #136 half: a tool name in prompt text outliving the
// tool. Every name the scan finds must be advertised by the pinned cli.
func checkToolNames(cfg config) error {
	lits, err := promptguard.Literals(cfg.root)
	if err != nil {
		return err
	}
	f, err := os.Open(cfg.manifestPath) //nolint:gosec // path is a CI-supplied fixed argument
	if err != nil {
		return fmt.Errorf("open tool manifest: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only
	advertised, err := promptguard.ManifestToolNames(f)
	if err != nil {
		return err
	}
	have := make(map[string]bool, len(advertised))
	for _, n := range advertised {
		have[n] = true
	}
	var missing []error
	for _, n := range promptguard.ToolNames(lits) {
		if !have[n] {
			missing = append(missing, fmt.Errorf("prompt text at %s names MCP tool %q, which the pinned tatara-cli does not advertise (%d tools in the manifest)", promptguard.Where(lits, n), n, len(advertised)))
		}
	}
	return errors.Join(missing...)
}

// checkRoster is the C1 half: delegationDirective's bullet list must equal what
// installAgents copies. A name only in the directive is an agent told to
// dispatch something that is not installed; a name only in the clone is an
// agent that lands in every pod and is never advertised to the agent that could
// use it.
func checkRoster(cfg config) error {
	roster, err := bootstrap.DelegationRoster()
	if err != nil {
		return err
	}
	installed, err := bootstrap.AgentNames(cfg.agentsDir)
	if err != nil {
		return err
	}
	var problems []error
	if extra := difference(roster, installed); len(extra) > 0 {
		problems = append(problems, fmt.Errorf("the delegation directive names typed subagents the pinned skills ref does not ship: %v", extra))
	}
	if missing := difference(installed, roster); len(missing) > 0 {
		problems = append(problems, fmt.Errorf("the pinned skills ref ships typed subagents the delegation directive does not name, so they install into every pod unadvertised: %v", missing))
	}
	return errors.Join(problems...)
}

// difference returns the sorted members of a that are not in b.
func difference(a, b []string) []string {
	inB := make(map[string]bool, len(b))
	for _, s := range b {
		inB[s] = true
	}
	var out []string
	for _, s := range a {
		if !inB[s] {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
