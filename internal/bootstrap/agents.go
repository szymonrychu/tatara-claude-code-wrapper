package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// installAgents copies typed subagent definitions (flat *.md files, one
// level deep - no subdirectories, no SKILL.md-style marker) from each
// AgentsSrc directory into <workspace>/.claude/agents. This mirrors
// installSkills's clone-then-copy path (same already-cloned skills-repo
// checkout, same later-source-wins flatten semantics) but does not
// profile-gate: the typed agents (explorer/tester/builder/architect) are a
// shared dispatch palette used by implement's rigid skill and by any other
// kind's Agent-tool subagent fan-out (brainstorm/incident/review alike, per
// the task-kind redesign's "subagents are still first-class" mandate), not a
// single profile's concern - and there are only four files, so the token
// cost of always installing all of them is negligible next to the skills
// corpus that profile-gating exists to bound.
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
func installAgentsFromSrc(src, dst string, p Params) (int, error) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return 0, fmt.Errorf("read dir %s: %w", src, err)
	}
	installed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
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
