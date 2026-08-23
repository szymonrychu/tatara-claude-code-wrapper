package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
)

// installAgents copies typed subagent definitions (flat *.md files, one
// level deep - no subdirectories, no SKILL.md-style marker) from each
// AgentsSrc directory into <workspace>/.claude/agents. This mirrors
// installSkills's clone-then-copy path (same already-cloned skills-repo
// checkout, same later-source-wins flatten semantics) but does not
// profile-gate: the typed agents are a shared dispatch palette used by
// implement's rigid skill and by any other kind's Agent-tool subagent fan-out
// (brainstorm/incident/review alike, per the task-kind redesign's "subagents
// are still first-class" mandate), not a single profile's concern - and it is
// a handful of files, so the token cost of always installing all of them is
// negligible next to the skills corpus that profile-gating exists to bound.
//
// This copies whatever the skills clone ships; delegationDirective is what
// names them to the agent. Those two rosters are reconciled by
// .github/ci/check-agent-facing-names.sh, so this comment deliberately carries
// no list of its own - a third copy could only drift.
func installAgents(p Params) error {
	dst := filepath.Join(p.Workspace, ".claude", "agents")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("mkdir agents: %w", err)
	}
	total := 0
	for _, src := range p.AgentsSrc {
		if src == "" {
			continue
		}
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		}
		n, err := installAgentsFromSrc(src, dst, p)
		if err != nil {
			return fmt.Errorf("install agents from %s: %w", src, err)
		}
		total += n
	}
	if p.Log != nil {
		p.Log.Info("agents installed", "action", "install_agents", "count", total)
	}
	if p.M != nil {
		p.M.AgentsInstalled.Add(float64(total))
	}
	return nil
}

// installAgentsFromSrc copies every top-level *.md file in src into dst.
// Unlike installSkillsFromSrc, this does not recurse: agent definitions ship
// as flat files at the plugin's .claude/agents/ root, not one-dir-per-item.
// The listing comes from agentFiles, which AgentNames also uses, so the guard
// that reconciles the roster enumerates exactly what this installs.
func installAgentsFromSrc(src, dst string, p Params) (int, error) {
	entries, err := agentFiles(src)
	if err != nil {
		return 0, err
	}
	installed := 0
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return installed, fmt.Errorf("stat %s: %w", e.Name(), err)
		}
		target := filepath.Join(dst, e.Name())
		if _, statErr := os.Stat(target); statErr == nil && p.Log != nil {
			p.Log.Info("agent shadowed", "action", "install_agents", "name", e.Name(), "src", src)
		}
		if err := copyFile(filepath.Join(src, e.Name()), target, info.Mode().Perm()); err != nil {
			return installed, fmt.Errorf("copy agent %s: %w", e.Name(), err)
		}
		installed++
	}
	return installed, nil
}
