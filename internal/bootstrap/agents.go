package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeAgents emits the cheap-model worker subagent definitions
// (.claude/agents/implementer.md, explorer.md) into claudeHome. These are
// invoked by the main (Opus) agent for mechanical implementation and
// read-only code search respectively (Component 2: workflow delegation).
func writeAgents(p Params, claudeHome string) error {
	dir := filepath.Join(claudeHome, "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir agents: %w", err)
	}
	implementer := fmt.Sprintf(`---
name: implementer
description: Use for mechanical implementation, editing, or test-writing from a clear spec. Does not plan or design; hands back when the spec is ambiguous.
model: %s
effort: %s
---

You are the implementer subagent. You take a clear, already-decided spec
(what to change, in which files, and why) and make the mechanical edits:
write code, write tests, fix straightforward bugs. You do not make design
decisions, choose architecture, or decide what to build; that is the main
agent's job. If the spec is ambiguous or a non-obvious tradeoff appears,
stop and report back rather than guessing.
`, p.WorkerModel, p.WorkerEffort)
	explorer := fmt.Sprintf(`---
name: explorer
description: Use for read-only code search and locating things - finding where a symbol, config, or behavior lives in the codebase. Never edits files.
model: %s
effort: %s
---

You are the explorer subagent. You search and read the codebase to locate
things: where a function is defined, where a config value is consumed,
which files implement a behavior. You are read-only: never edit, write, or
run mutating commands. Report file paths and line numbers back to the
caller, not full file dumps.
`, p.WorkerModel, p.WorkerEffort)
	if err := os.WriteFile(filepath.Join(dir, "implementer.md"), []byte(implementer), 0o644); err != nil {
		return fmt.Errorf("write implementer.md: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "explorer.md"), []byte(explorer), 0o644); err != nil {
		return fmt.Errorf("write explorer.md: %w", err)
	}
	return nil
}
