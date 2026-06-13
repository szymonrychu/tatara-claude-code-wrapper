---
name: tatara-deep-research
description: Use on an autonomous platform-research turn (the brainstorm task kind) to discover ONE high-leverage improvement for the tatara platform and open a discovery-phase issue via the propose_issue MCP tool. Researches deeply across the whole platform using the tatara-memory knowledge/code graph plus the on-disk repo, scores leverage against platform and per-repo goals, and files a single well-formed issue that stays in discovery (never self-implemented).
---

# tatara deep research

Discover and propose ONE high-leverage improvement issue per run. All
input and output go through the `tatara` MCP server. You never use git or
gh; you never open an issue yourself - `propose_issue` does that under the
bot identity.

## Hard constraints

- ONE issue per run. The brainstorm task completes after a single proposal.
- Stay in discovery. Do NOT request implementation. Embed the literal
  marker `<!-- tatara-authored -->` in the issue body and never set a
  trigger label - the operator holds tatara-authored ideas in
  conversation until a human approves.
- Every proposal must respect the platform's 14 hard rules (read the
  on-disk `CLAUDE.md`), or the loop that later implements it will reject
  it. KISS; no tech debt; charts cluster-agnostic; conventional commits;
  newest stable Go; JSON slog + INFO business logging + /metrics.
- Communication only via `tatara` MCP tools.

The `tatara` tools auto-scope to your current task and project from the pod
environment. Do NOT try to pass an environment variable as an argument
(you cannot expand it) - omit the `task`/`project` args and the tool fills
them in. The repo slug and project name you need are printed in your turn
prompt; the memory `code_*`/`query` tools take an explicit `repo=<slug>`.

## Workflow

Create a TodoWrite item per numbered step.

1. **Orient on goals.** The Project's repos are cloned under
   `/workspace/<owner>/<repo>` (e.g. `/workspace/szymonrychu/tatara-operator`);
   run `ls /workspace/*/` to list them. Your turn prompt names the target
   repo. Read that repo's on-disk `ROADMAP.md`, `MEMORY.md`, and `CLAUDE.md`
   (the platform goal, the repo charter, the hard rules). Then use the
   memory MCP tools for the wider picture: `query` (mode global or hybrid)
   for "tatara platform goal" and "open roadmap themes"; `describe` for an
   overview of the target repo.

2. **Map current state.** Use the code-graph tools to find where the
   system is fragile or under-optimized, repo-scoped where useful:
   `code_stats`, `code_important` (high-PageRank entities = load-bearing
   code), `code_communities` (subsystem clustering), `code_bridges`
   (coupling/risk), and `code_cross_repo` (cross-repo edges - the pod has
   only one repo on disk, so cross-repo understanding MUST come from the
   graph). Then READ the actual on-disk code for the strongest candidate
   area to confirm what the graph suggested.

3. **Score leverage.** Rank candidate improvements by impact in this
   order: (a) reliability/observability of the LIVE autonomous loop
   (it is dogfooding in production and surfaces real bugs); (b) un-built
   but planned loop features; (c) the Phase-9 SOTA backlog; (d) deploy
   debt. Respect gates: do NOT propose downstream memory ranking/reranker
   work before the memory retrieval-quality eval harness exists. Pick the
   single highest-leverage, well-scoped item.

4. **Dedup.** Call `task_list` and review the repo's open issues/tasks to
   avoid duplicating an existing proposal or the operator's own brainstorm
   output. If a similar idea is already open, pick the next-best candidate
   instead.

5. **Compose ONE proposal.** Write:
   - Title: imperative, specific (e.g. "Add per-item ingest timeout to the
     memory ingest worker").
   - Body: Problem (what hurts, why it matters to the platform/repo goal);
     Evidence (`file:line` references and concrete graph findings from
     steps 1-2); Proposed approach (KISS, respecting the hard rules);
     Scope boundary (what is in and explicitly out); Open questions for
     the maintainer. Append the literal line `<!-- tatara-authored -->`.

6. **File it.** Call `propose_issue` with `title`, `body`, `kind`
   (`improvement` or `bug`), and `repo` (the repo slug; `project` defaults
   from env). Do not set any trigger/approval label. Then stop - the
   brainstorm task is complete.

## Anti-patterns

- Proposing more than one issue in a run.
- Proposing vague "improve X" issues with no `file:line` evidence.
- Requesting implementation / setting a trigger label (breaks discovery).
- Proposing memory ranking work before the eval-harness gate.
- Reading only the on-disk repo and ignoring the cross-repo graph.
