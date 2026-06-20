---
name: tatara-health-check
description: Use on an autonomous project-health-check turn (the healthCheck task kind) to survey a project's repos and surface ONE high-leverage health issue - a CI failure, a coverage gap, code worth simplifying, a CI/CD pipeline step worth adding, or other tech-debt - then open a single targeted discovery-phase issue via the propose_issue MCP tool. Researches via the tatara-memory graph plus the on-disk repo, dedups against open issues, and files one well-scoped issue that stays in discovery (never self-implemented).
---

# tatara health check

Survey the health of a project's repositories and propose ONE
high-leverage, well-targeted fix per run. All input and output go through
the `tatara` MCP server. You never use git or gh; you never open an issue
yourself - `propose_issue` does that under the bot identity.

This is the operational sibling of `tatara-deep-research`: deep-research
hunts for the next platform improvement; health-check hunts for concrete
decay in what already exists. Same discovery discipline, narrower lens.

## Hard constraints

- ONE issue per run. The healthCheck task completes after a single proposal.
- Stay in discovery. Do NOT request implementation. Embed the literal
  marker `<!-- tatara-authored -->` in the issue body and never set a
  trigger label - the operator holds tatara-authored ideas in
  conversation until a human approves.
- Tightly scoped, single-cause proposals only. A health finding names ONE
  concrete defect with evidence and ONE fix - not a grab-bag audit. A
  vague "improve test coverage" issue is worthless; "the memory ingest
  worker's retry path (worker.go:140-180) has no test and regressed in
  #34" is actionable.
- Every proposal must respect the platform's hard rules (read the on-disk
  `CLAUDE.md`), or the loop that later implements it will reject it. KISS;
  no tech debt; charts cluster-agnostic; conventional commits; newest
  stable Go; JSON slog + INFO business logging + /metrics.
- Communication only via `tatara` MCP tools.

## Orchestration (run at maximum effort)

This is a multi-repo health survey - run it at **maximum effort** and
orchestrate, do not work single-threaded:

- The pod's `EFFORT` is already set high; sustain deep multi-step reasoning and
  reproduce failures before deciding. Spend the thinking budget on the survey.
- **Decompose** the survey into one independent unit of work per repository in
  the Project (the repos under `/workspace/*/` plus the cross-repo graph view).
- **Dispatch one parallel subagent per repo** to probe that repo's five health
  dimensions (CI failures via `mise run test`/`lint`, coverage gaps, code to
  simplify, missing pipeline steps, other tech-debt) and its `code_*` graph
  signal. Launch them in a single batch so they run concurrently.
- Use a **Workflow** to fan the per-repo probes out and then **synthesize** their
  findings: prefer a systemic health issue recurring across >=2 repos or a
  platform-wide pipeline gap over a single-repo decay, then pick the ONE
  highest-leverage, well-evidenced finding.
- Only after synthesis do you compose the proposal below.

The `tatara` tools auto-scope to your current task and project from the pod
environment. Do NOT try to pass an environment variable as an argument
(you cannot expand it) - omit the `task`/`project` args and the tool fills
them in. The repo slug and project name you need are printed in your turn
prompt; the memory `code_*`/`query` tools take an explicit `repo=<slug>`.

## Health dimensions

Score every candidate against these five dimensions (issue #56). Pick the
single most impactful, well-evidenced one:

1. **CI failures.** Read the on-disk CI config (`.github/workflows/*.yml`,
   `.gitlab-ci.yml`, `Makefile`, `.mise.toml` tasks). Run `mise run test`
   / `mise run lint` for the target repo and observe what actually fails
   or flakes. A red or flaky pipeline is the highest-priority finding.
2. **Code coverage gaps.** Identify load-bearing code with no test. Use
   `code_important` (high-PageRank entities) and `code_bridges`
   (coupling/risk) to find what is both critical AND untested; confirm by
   reading the code and its `_test.go` neighbours on disk. Prefer a
   specific untested critical path over an aggregate percentage.
3. **Code to simplify.** Use `code_communities`/`code_bridges` and on-disk
   reading to find genuine accidental complexity - a god-function, a
   premature abstraction, duplicated logic the hard rules say to collapse.
   Only propose a simplification with a clear, bounded diff.
4. **Pipeline steps worth adding.** A missing but cheap CI/CD guardrail:
   no lint stage, no race detector, no coverage gate, no chart-lint, no
   vulnerability/dependency scan, no image build check. Propose ONE
   concrete step, matched to how that repo's pipeline is already wired.
5. **Other tech-debt.** Anything the repo's own `MEMORY.md` flags as a
   deferred cleanup, a deprecated-but-kept shim ready to remove, a
   `TODO`/`FIXME` with real cost, or a known latent bug noted in passing.

## Workflow

Create a TodoWrite item per numbered step.

1. **Orient on goals.** The Project's repos are cloned under
   `/workspace/<owner>/<repo>` (e.g. `/workspace/szymonrychu/tatara-operator`);
   run `ls /workspace/*/` to list them. Your turn prompt names the target
   repo. Read that repo's on-disk `ROADMAP.md`, `MEMORY.md`, and
   `CLAUDE.md` (the repo charter, deferred-cleanup notes, the hard rules).

2. **Probe health.** Walk the five dimensions above for the target repo.
   Combine three evidence sources: the on-disk CI config and code; an
   actual `mise run test`/`mise run lint` run to see real failures; and
   the memory `code_*` tools (`code_stats`, `code_important`,
   `code_bridges`, `code_communities`) to locate critical-yet-fragile or
   complex areas. Cross-repo health signal MUST come from the graph
   (`code_cross_repo`) since the pod has only one repo on disk.

3. **Score and pick ONE.** Rank candidates by: (a) it is breaking CI or
   the live autonomous loop right now; (b) it removes real, recurring
   risk on a load-bearing path; (c) it is a cheap, high-value guardrail;
   (d) it is bounded, low-risk cleanup. Pick the single highest-leverage,
   well-scoped item with concrete `file:line` evidence.

4. **Dedup.** Call `task_list` and review the repo's open issues/tasks to
   avoid duplicating an existing proposal or the operator's own brainstorm
   output. If a similar finding is already open, either pick the next-best
   candidate or, if your finding genuinely refines an open issue, call
   `comment` on that issue instead of opening a new one.

5. **Compose ONE proposal.** Write:
   - Title: imperative, specific (e.g. "Add a race-detector stage to the
     tatara-memory test pipeline" or "Cover the ingest retry path that
     regressed in #34").
   - Body: Problem (the concrete defect and why it hurts the repo/platform
     goal); Evidence (`file:line` references, the failing command output,
     and graph findings from steps 1-2); Proposed fix (KISS, respecting
     the hard rules, with the bounded diff sketched); Scope boundary (what
     is in and explicitly out); a SINGLE explicit decision for the
     maintainer: "Approve to implement, or comment to refine." Do NOT list
     open questions that invite back-and-forth.
     Append the literal line `<!-- tatara-authored -->`.

6. **File it.** Call `propose_issue` with `title`, `body`, `kind`
   (`bug` for a CI failure / latent defect, `improvement` for coverage /
   simplification / pipeline / cleanup), and `repo` (the repo slug;
   `project` defaults from env). Do not set any trigger/approval label.
   Then stop - the healthCheck task is complete.

## Anti-patterns

- Proposing more than one issue in a run.
- Aggregate or vague findings ("raise coverage", "reduce complexity")
  with no single `file:line` cause and no bounded fix.
- Reporting a CI failure you never reproduced (`mise run test`/`lint`).
- Requesting implementation / setting a trigger label (breaks discovery).
- Reading only the on-disk repo and ignoring the code graph and CI config.
- Duplicating an open issue instead of commenting on it or moving on.
