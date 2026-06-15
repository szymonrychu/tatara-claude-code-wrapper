---
name: tatara-repo-health
description: Use on an autonomous repository-health turn (the healthCheck task kind) to audit ONE target repo's health - CI/pipeline failures, missing pipeline steps, test-coverage gaps, code worth simplifying, and other tech debt - and open ONE targeted discovery-phase issue via the propose_issue MCP tool. Repo-scoped and maintenance-focused (distinct from tatara-deep-research, which hunts whole-platform feature leverage). Files a single well-formed issue that stays in discovery, never self-implemented.
---

# tatara repo health

Audit the health of ONE target repo and propose ONE targeted maintenance
issue per run. All input and output go through the `tatara` MCP server.
You never use git or gh; you never open an issue yourself - `propose_issue`
does that under the bot identity.

This skill is the maintenance sibling of `tatara-deep-research`. That skill
hunts the single highest-leverage *feature* improvement across the whole
platform; this one stays inside ONE repo and looks only at its *health*:
failing or flaky CI, pipelines missing a valuable step, thin test
coverage, code that is more complex than it needs to be, and accumulated
tech debt. Pick whichever lens surfaces the strongest, best-scoped finding
for the target repo.

## Hard constraints

- ONE issue per run. The healthCheck task completes after a single proposal.
- Stay in discovery. Do NOT request implementation. Embed the literal
  marker `<!-- tatara-authored -->` in the issue body and never set a
  trigger label - the operator holds tatara-authored ideas in
  conversation until a human approves.
- Repo-scoped. Audit only the target repo named in your turn prompt. Do
  not propose cross-repo or platform-wide refactors - that is
  `tatara-deep-research`'s job; defer to it and pick a repo-local finding.
- Targeted, not sweeping. One concrete, well-bounded fix with `file:line`
  evidence - never "improve test coverage" or "clean up the code" in the
  abstract.
- Every proposal must respect the platform's hard rules (read the on-disk
  `CLAUDE.md`): KISS; no new tech debt; charts cluster-agnostic;
  conventional commits; newest stable Go; JSON slog + INFO business
  logging + /metrics.
- Communication only via `tatara` MCP tools.

The `tatara` tools auto-scope to your current task and project from the pod
environment. Do NOT try to pass an environment variable as an argument
(you cannot expand it) - omit the `task`/`project` args and the tool fills
them in. The repo slug and project name you need are printed in your turn
prompt; the memory `code_*`/`query` tools take an explicit `repo=<slug>`.

## Workflow

Create a TodoWrite item per numbered step.

1. **Orient on the target repo.** The Project's repos are cloned under
   `/workspace/<owner>/<repo>` (e.g. `/workspace/szymonrychu/tatara-operator`);
   run `ls /workspace/*/` to list them. Your turn prompt names the ONE
   target repo for this run. Read that repo's on-disk `ROADMAP.md`,
   `MEMORY.md`, and `CLAUDE.md` (the goal, the charter, the hard rules),
   then `describe` (mode local, `repo=<slug>`) for an overview.

2. **Probe health across the five lenses.** Gather concrete evidence;
   don't guess:
   - **CI / pipeline failures:** read the pipeline config on disk
     (`.github/workflows/*`, `.gitlab-ci.yml`, `Makefile`, `Dockerfile`).
     Look for recently red or flaky jobs, retries, and skipped steps.
   - **Missing pipeline steps:** compare against the platform norm - is
     there lint, `go vet`, race-detector tests, coverage reporting, image
     scanning, a test-guard stage? Note a valuable step that is absent.
   - **Coverage gaps:** find load-bearing code with no or thin tests. Use
     `code_important` (high-PageRank entities) and `code_stats`
     (`repo=<slug>`), then check which of those have no adjacent `_test`
     coverage on disk.
   - **Code to simplify:** use `code_bridges` and `code_communities`
     (`repo=<slug>`) to find tangled / high-coupling spots, then READ the
     on-disk code to confirm a genuine, bounded simplification.
   - **Other tech debt:** TODO/FIXME clusters, deprecated fields still in
     use, duplicated logic, dead code.

3. **Score and pick ONE.** Rank candidate findings by: (a) risk to the
   LIVE autonomous loop (it dogfends in production); (b) how bounded and
   unambiguous the fix is; (c) how much future toil it removes. Prefer a
   small, certain, high-signal fix over a large speculative one. Pick the
   single best finding for this repo.

4. **Dedup.** Call `task_list` and review the repo's open issues/tasks to
   avoid duplicating an existing proposal, an open health finding, or the
   brainstorm output. If a similar item is already open, pick the
   next-best candidate instead; if nothing healthy-and-new remains, file
   nothing and stop.

5. **Compose ONE proposal.** Write:
   - Title: imperative, specific, and lens-tagged (e.g. "Add
     race-detector step to tatara-cli test pipeline" or "Cover
     finishTriage self-approval guard with a unit test").
   - Body: Problem (what is unhealthy and why it matters to the
     repo/platform goal); Evidence (`file:line` references and concrete
     graph/CI findings from step 2); Proposed approach (KISS, respecting
     the hard rules); Scope boundary (what is in and explicitly out); a
     SINGLE explicit decision for the maintainer: "Approve to implement,
     or comment to refine." Do NOT list open questions that invite
     back-and-forth - one well-researched proposal gets one clear approval
     gate. Append the literal line `<!-- tatara-authored -->`.

6. **File it.** Call `propose_issue` with `title`, `body`, `kind`
   (`bug` for a failing/flaky pipeline, otherwise `improvement`), and
   `repo` (the target repo slug; `project` defaults from env). Do not set
   any trigger/approval label. Then stop - the healthCheck task is
   complete.

## Anti-patterns

- Proposing more than one issue in a run.
- Sweeping, unscoped asks ("improve coverage", "reduce tech debt") with no
  `file:line` evidence.
- Proposing a feature / cross-repo refactor (that is
  `tatara-deep-research`'s lane) instead of a repo-local health fix.
- Requesting implementation / setting a trigger label (breaks discovery).
- Filing a duplicate of an already-open health or brainstorm proposal.
- Judging CI health from memory instead of reading the on-disk pipeline
  config.
