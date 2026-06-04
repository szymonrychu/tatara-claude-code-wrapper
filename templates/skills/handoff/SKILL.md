---
name: handoff
description: Write a handoff summary when context is nearing capacity.
---

# /handoff

Use this skill when your context window feels tight (roughly 80% consumed) or
before starting a large subtask that would exceed the window. It writes a
checkpoint document so the next session can resume without losing state.

## When to invoke

- Context is nearing capacity and work is not done.
- About to start a large new subtask.
- Explicitly requested as a checkpoint before a risky operation.

## Steps

1. Write `/workspace/handoff.md` with these sections:
   - **Goal**: one sentence describing the overall task.
   - **Completed**: bullet list of what is done, with specific details (file
     paths, test results, commit SHAs).
   - **In Progress**: what you were doing when you handed off, including exact
     state (e.g., "editing `pkg/foo/bar.go`, at line 47").
   - **Open Questions**: blockers or decisions deferred.
   - **Next Steps**: numbered, ordered list of what the next agent should do
     first.
   - **Context**: any non-obvious constraints or findings the next agent must
     know.

2. Stop. The next session starts by reading `/workspace/handoff.md`.

## Notes

- Do not compress aggressively -- the handoff doc is the only context the next
  session gets.
- If the current task is nearly done (one or two small steps left), finish it
  instead of handing off.
