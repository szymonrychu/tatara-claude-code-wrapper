---
name: tatara-research-followup
description: Use when continuing an existing discovery/research issue conversation on an issueLifecycle Triage or Conversation turn. Read the issue thread and task state, research the gaps with the tatara-memory graph and on-disk code, post substantive design comments via the comment MCP tool, refine the proposal into a concrete design, and push toward human approval - never self-approving. Idle quietly when there is nothing new to add.
---

# tatara research follow-up

Keep a discovery-phase issue conversation alive and move it toward an
approvable design. All input and output go through the `tatara` MCP
server. You never use git or gh.

## Hard constraints

- NEVER self-approve. If THIS issue is tatara-authored, only a human's
  approval comment may lead to implementation - you only discuss and
  refine. End the turn with `issue_outcome(discuss)`, never
  `issue_outcome(implement)` on an unapproved tatara-authored issue.
- Silence over noise. If there is no human input and nothing genuinely
  new to add, post nothing and let the conversation idle.
- One focused turn. Communication only via `tatara` MCP tools.

## Workflow

Create a TodoWrite item per numbered step.

1. **Load context.** Call `task_get` (task=env `TATARA_TASK`) for the task
   status and lifecycle state. Read the issue body and the full comment
   thread (the turn prompt includes the thread). Extract: open questions,
   maintainer asks, unresolved design decisions, and whether a human has
   engaged.

2. **Research the gaps.** Use the memory MCP tools (`query`, `describe`,
   and the `code_*` family incl. `code_cross_repo`) plus the on-disk code
   to answer the specific questions raised and to deepen any thin part of
   the proposal. The pod has one repo on disk; use the graph for
   cross-repo facts.

3. **Respond in-thread** with the `comment` MCP tool (task=env
   `TATARA_TASK`, body=...). Post focused comments, not one wall of text:
   - Answer each maintainer question with evidence (`file:line`, graph
     findings).
   - Refine the proposal into a concrete design: architecture,
     components, data flow, error handling, testing, plus an
     implementation outline.
   - Surface remaining decisions for the maintainer.

4. **Drive to approval.** When the design is converged AND a human has
   engaged in the thread, post a short summary of the agreed design and
   explicitly ask the maintainer for the approval signal (an approval
   comment / the approval label). Do not approve it yourself.

5. **Idle discipline.** If nothing new is warranted, do not comment.

6. **Close the turn.** Call `issue_outcome` with action `discuss` (supply
   a one-line status as `comment`) to hold the issue in Conversation. Use
   action `close` ONLY if the idea is clearly dead AND a human concurred
   in the thread. You MUST call `issue_outcome` before finishing.

## Anti-patterns

- Calling `issue_outcome(implement)` on a tatara-authored issue without a
  human approval comment.
- Posting one giant comment instead of focused, answerable ones.
- Commenting with no new research when the thread is waiting on the human.
- Making code changes or opening PRs in this turn.
