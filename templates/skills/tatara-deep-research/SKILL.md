---
name: tatara-deep-research
description: Use on an autonomous platform-research turn (the brainstorm task kind) to discover ONE high-leverage improvement for the tatara platform. Researches deeply across the whole platform using the tatara-memory knowledge/code graph plus the on-disk repo, scores leverage, dedups against open issues, then takes exactly one action: open a discovery-phase issue via propose_issue when the idea is novel and standalone, or add a substantive design comment via comment_on_issue when the idea connects to / is a sub-aspect of an existing open issue. Never self-implemented.
---

# tatara deep research

Discover ONE high-leverage improvement per run, then take exactly one
action: `propose_issue` (novel, standalone) or `comment_on_issue` (the
idea belongs on an existing open issue). All input and output go through
the `tatara` MCP server. You never use git or gh; you never open or comment
on an issue yourself - `propose_issue` / `comment_on_issue` do that under
the bot identity.

## Hard constraints

- ONE action per run: EITHER one `propose_issue` (novel, standalone idea)
  OR one `comment_on_issue` (your idea duplicates, extends, or is a
  sub-aspect of an existing open issue). The brainstorm task completes after
  that single action. If nothing is genuinely novel AND nothing on an open
  issue is worth a substantive addition, finish with no action - honest
  no-yield beats a low-value proposal or an empty comment.
- Stay in discovery. Do NOT request implementation. Embed the literal
  marker `<!-- tatara-authored -->` in the issue body and never set a
  trigger label - the operator holds tatara-authored ideas in
  conversation until a human approves.
- Every proposal must respect the platform's 14 hard rules (read the
  on-disk `CLAUDE.md`), or the loop that later implements it will reject
  it. KISS; no tech debt; charts cluster-agnostic; conventional commits;
  newest stable Go; JSON slog + INFO business logging + /metrics.
- Communication only via `tatara` MCP tools.

## Orchestration (run at maximum effort)

This is a deep, cross-repo research turn - run it at **maximum effort** and
orchestrate, do not work single-threaded:

- The pod's `EFFORT` is already set high; sustain deep multi-step reasoning and
  read widely before deciding. Spend the thinking budget on the survey.
- **Decompose** the cross-repo survey into one independent unit of work per
  repository in the Project (the repos under `/workspace/*/` plus the cross-repo
  graph view).
- **Dispatch one parallel subagent per repo** to gather that repo's state
  (roadmap themes, fragile/load-bearing code via the `code_*` graph tools, open
  issues/MRs, recurring debt). Launch them in a single batch so they run
  concurrently; do not serialize what can fan out.
- Use a **Workflow** to fan the per-repo investigations out and then **synthesize**
  their findings into the single highest-leverage SYSTEMIC opportunity - a
  pattern spanning >=2 repos, a platform-wide gap, or recurring debt - in
  preference to a one-repo tweak.
- Only after synthesis do you choose the propose-vs-comment action below. For a
  genuinely systemic improvement you MAY open one `propose_issue` per affected
  repo sharing a single `systemicId` you generate (bounded, <=6); the operator
  correlates them and counts the group as one against the proposal cap.

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

4. **Dedup, then decide propose-vs-comment.** Call `task_list` and review
   the repo's open issues/tasks (and any open issues listed in your turn
   prompt) before acting:
   - **Duplicate** of an already-open issue -> do NOT open a new one. Either
     pick the next-best novel candidate, or, if you have a concrete addition,
     comment on it (next bullet).
   - **Connecting / sub-aspect / extends** an existing open issue (it belongs
     there, not as its own issue) -> call `comment_on_issue` with a
     substantive design note that advances that issue (`repo` = the slug,
     `number` = the issue number, `body` = the note). This ENDS the run -
     skip steps 5-6.
   - **Genuinely novel and standalone** -> proceed to compose a new proposal
     (step 5).

5. **Compose ONE proposal.** Write:
   - Title: imperative, specific (e.g. "Add per-item ingest timeout to the
     memory ingest worker").
   - Body: Problem (what hurts, why it matters to the platform/repo goal);
     Evidence (`file:line` references and concrete graph findings from
     steps 1-2); Proposed approach (KISS, respecting the hard rules);
     Scope boundary (what is in and explicitly out); a SINGLE explicit
     decision for the maintainer: "Approve to implement, or comment to
     refine." Do NOT list open questions that invite back-and-forth - one
     well-researched proposal gets one clear approval gate.
     Append the literal line `<!-- tatara-authored -->`.

6. **File it** (novel path only; a connecting idea already ended at step 4
   via `comment_on_issue`). Call `propose_issue` with `title`, `body`, `kind`
   (`improvement` or `bug`), and `repo` (the repo slug; `project` defaults
   from env). Do not set any trigger/approval label. Then stop - the
   brainstorm task is complete.

## Anti-patterns

- Proposing more than one issue (or more than one action) in a run.
- Discarding a connecting idea instead of adding it as a `comment_on_issue`
  on the existing issue it belongs to (opening a near-duplicate is worse).
- Proposing vague "improve X" issues with no `file:line` evidence.
- Requesting implementation / setting a trigger label (breaks discovery).
- Proposing memory ranking work before the eval-harness gate.
- Reading only the on-disk repo and ignoring the cross-repo graph.
