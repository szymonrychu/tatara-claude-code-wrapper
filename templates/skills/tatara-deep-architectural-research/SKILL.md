---
name: tatara-deep-architectural-research
description: Use on an architectural-research turn to discover ONE high-leverage structural improvement for the tatara platform. Walks SCOPE -> MAP INWARD -> (Phase-1 stubbed) SURVEY -> ASSESS FIT -> NOVELTY -> SYNTHESIZE ADR -> PROPOSE; Phase-1 external field survey deferred (memory graph + on-disk only); terminal no-yield action is skip_research; never self-implemented.
---

# tatara deep architectural research

Discover ONE high-leverage structural improvement per run, produce an
ADR/RFC artifact, then file one `propose_issue` (or `skip_research` when
nothing novel or shippable emerges). All input and output go through the
`tatara` MCP server. You never use git or gh directly; you never open an
issue yourself - `propose_issue` does that under the bot identity.

## Hard constraints

- ONE outcome per run: either `propose_issue` (novel, shippable) or
  `skip_research(reason)` (honest no-yield). No partial outputs, no
  implementation requests.
- Discovery-only. Embed `<!-- tatara-authored -->` in the issue body; set no
  trigger label. The operator parks tatara-authored ideas in Conversation
  until a human approves. Never self-implement an unapproved idea.
- Respect every platform hard rule (read the on-disk `CLAUDE.md`): KISS, no
  tech debt, charts cluster-agnostic, conventional commits, newest stable Go,
  JSON slog + INFO business logging + /metrics.
- Communication only via `tatara` MCP tools.
- Terminal no-yield action is `skip_research(reason)` - call it to honestly
  end a research turn that surfaces nothing novel and shippable. Post the
  reason, exactly as `decline_implementation` does. Wrapper does not
  implement this tool; tatara-cli serves it.
- Use the ADR template and Technology Radar convention in
  [`adr-template.md`](adr-template.md) for the SYNTHESIZE artifact.

## Orchestration (run at maximum effort)

This is a deep, cross-repo architectural research turn - run it at
**maximum effort** and orchestrate, do not work single-threaded:

- The pod's `EFFORT` is already set high; sustain deep multi-step reasoning
  and read widely before deciding.
- **Decompose** the cross-repo survey into one independent unit of work per
  repository in the Project (repos under `/workspace/*/` plus the cross-repo
  graph view).
- **Dispatch one parallel subagent per repo** to gather that repo's state
  (MEMORY themes, fragile/load-bearing code via `code_*` graph tools, open
  issues/MRs, recurring debt). Launch them in a single batch so they run
  concurrently; do not serialize what can fan out.
- Use a **Workflow** to fan the per-repo investigations out and then
  **synthesize** their findings into the single highest-leverage SYSTEMIC
  opportunity - a pattern spanning >=2 repos, a platform-wide gap, or
  recurring debt - in preference to a one-repo tweak.
- For a genuinely systemic improvement you MAY open one `propose_issue` per
  affected repo sharing a single `systemicId` you generate (bounded, <=6);
  the operator correlates them and counts the group as one against the
  proposal cap.

The `tatara` tools auto-scope to your current task and project from the pod
environment. Do NOT try to pass an environment variable as an argument
(you cannot expand it) - omit the `task`/`project` args and the tool fills
them in. The repo slug and project name you need are printed in your turn
prompt; the memory `code_*`/`query` tools take an explicit `repo=<slug>`.

## Workflow

Create a TodoWrite item per numbered step.

1. **SCOPE** - pick ONE pain-point from `repoStateCtx` / MEMORY / ROADMAP /
   a failing fitness function. State it as a problem, not a solution (e.g.
   "direct github SDK imports in operator core block adding GitLab support"
   not "add GitLab"). Read the Project's repos via `ls /workspace/*/`, then
   read each repo's `MEMORY.md`, `ROADMAP.md`, and `CLAUDE.md`. Use the
   memory MCP `query` (mode global or hybrid) for "tatara platform goal" and
   "open roadmap themes" to situate the problem in context.

2. **MAP INWARD** - establish what tatara does today and where the
   coupling/debt lives. Use the code-graph tools: `code_stats`,
   `code_important` (high-PageRank load-bearing entities), `code_communities`
   (subsystem clusters), `code_bridges` (coupling/risk seams), and
   `code_cross_repo` (cross-repo edges). The pod has one repo on disk; cross-
   repo understanding MUST come from the graph. Then read the actual on-disk
   code for the strongest candidate area to confirm what the graph suggested.
   Dispatch one parallel subagent per repo for concurrent coverage.

3. **SURVEY THE FIELD** - **STUBBED for Phase 1.** External web/academic
   research is not yet wired (Phase 2). For now, survey only the memory
   graph and on-disk repos for prior art and comparable patterns already
   inside tatara. Record "field survey: external sources not yet available"
   as an explicit open question to carry into the ADR. Do not attempt
   WebSearch or WebFetch. In Phase 2 this step will activate outbound search
   (arXiv, OpenAlex, web) to find existing systems and papers that address
   this class of problem.

4. **ASSESS FIT** - score candidates against tatara's hard constraints:
   frozen model, headless, GitOps-only, KISS, no tech-debt. Reject anything
   needing weight updates or live self-patch. Produce 2-3 surviving options
   with explicit tradeoffs. Prefer strangler-fig approaches (behavioral-
   preserving, reversible, incrementally shippable) over big-bang rewrites.

5. **NOVELTY + LEARN** - OMNI-style gate. Is this genuinely novel vs past
   proposals (call `task_list`, review open issues in the turn prompt)? And
   is it shippable now given the repo state? If neither - call
   `skip_research(reason)` and stop. A near-duplicate or a proposal blocked
   by an unmet prerequisite does not advance the platform.

6. **SYNTHESIZE** - produce an ADR artifact following the template in
   [`adr-template.md`](adr-template.md): problem statement, evidence
   (`file:line` references + graph findings from steps 1-2), 2-3 options with
   on-disk citations (Phase-1) and a recommended option, a strangler-fig
   migration sketch, and the fitness function (CI check) that would gate the
   decision over time. Open questions are explicitly ALLOWED here - including
   the carried "field survey: external sources not yet available" follow-up
   from step 3. This is the ADR/RFC artifact that outlives the turn.

7. **PROPOSE** - file the ADR-backed proposal via `propose_issue` (one per
   affected repo sharing a single `systemicId` you generate, bounded <=6,
   for multi-repo systemic work). Include the full ADR text in the issue body.
   Embed `<!-- tatara-authored -->`. Set no trigger label - the operator parks
   it in Conversation for human approval. Then stop.

## Anti-patterns

- Proposing more than one action (propose OR skip, never both).
- Self-implementing or requesting implementation of a tatara-authored issue.
- Setting a trigger label that bypasses the human-approval gate.
- Proposing a vague "improve X" issue with no `file:line` evidence.
- Attempting WebSearch/WebFetch in Phase 1 (egress is not yet wired).
- Proposing memory ranking work before the eval-harness gate exists.
- Reading only the on-disk repo and ignoring the cross-repo graph.
- Producing an issue body that lists open questions (they go in the ADR
  artifact; the issue body has one well-researched decision for approval).
