---
name: tatara-deploy-harness
description: Use when a tatara agent is asked to deliver a GitHub issue end-to-end in a component repo - implement the code, ship the component MR, then open/merge a tatara-helmfile MR, watch the apply pipeline, roll back on failure, and close the issue as delivered. Triggers on a kickoff prompt naming an issue number and component repo.
---

# tatara-deploy-harness

## Overview

Rigid 9-state machine (S1..S9) for delivering one issue from triage to a
live deploy, autonomously, gated only by the diff and green pipelines. You run
ALL states in this one long-lived session. The implement sub-loop (S3) is
subagent-driven in a git worktree. Pipeline watching is `gh`. Issue research is
tatara-mcp + web.

**Core principle:** the loop target is S3. ANY downstream failure jumps back to
S3 (re-implement), never sideways. Apply failures (S7/S8) FIRST roll back the
deployed state, THEN jump to S3.

**Violating the letter of the state machine is violating the spirit.** Do not
skip a state, do not merge without watching its pipeline, do not declare
delivered without S9's `issue_outcome`.

## Inputs (already in your environment)

- Issue number + component repo: from the kickoff prompt (e.g. "deliver issue
  #N in tatara-cli"). If absent, read `gh issue list --repo szymonrychu/<repo>`.
- `TATARA_TASK`, `TATARA_PROJECT`: env, set by the operator. The `issue_outcome`
  MCP tool reads them; you do not pass them.
- `tatara-helmfile`: cloned into the workspace at
  `/workspace/szymonrychu/tatara-helmfile` (the operator put it in
  `TATARA_REPOS` because it is enrolled in the Project). If the dir is missing,
  fall back: `gh repo clone szymonrychu/tatara-helmfile /workspace/szymonrychu/tatara-helmfile`.
- The component repo is cloned at `/workspace/szymonrychu/<repo>`.

## Auth

`gh` and `git` are authed by `$GIT_TOKEN` (the szymonrychu-bot PAT). Export it
for `gh` once at the start of S1:

```bash
export GH_TOKEN="$GIT_TOKEN"
```

All `gh` commands below assume `--repo szymonrychu/<repo>` when not run inside a
clone. Prefer running inside the relevant clone so `--repo` is inferred.

## Idempotency / re-entry

This session may restart mid-loop (fresh pod resuming from
`/workspace/handoff.md`). BEFORE acting in any state, observe current state and
skip work already done:

| Check | Command | If already done |
|-------|---------|-----------------|
| Issue closed? | `gh issue view N --json state -q .state` | nothing to do; stop |
| Research comment posted? | `gh issue view N --json comments -q '.comments[].body'` grep your marker | skip S2 |
| Component PR exists? | `gh pr list --repo szymonrychu/<repo> --head <branch> --json number,state` | resume at S4/S5 |
| Component PR merged? | `gh pr view <n> --json state -q .state` == MERGED | skip to S6 |
| Helmfile PR exists/merged? | `gh pr list --repo szymonrychu/tatara-helmfile ...` | resume at S7/S8 |
| Apply run done? | `gh run list --repo szymonrychu/tatara-helmfile --workflow apply.yaml --json status,conclusion` | resume at S8/S9 |

Mark each comment you post with an HTML marker (`<!-- harness:S2 -->`) so the
re-entry grep is exact.

## Handoff checkpointing

At every state boundary, if context is tight, REQUIRED SUB-SKILL: invoke
handoff to write `/workspace/handoff.md` (Goal, Completed with issue/PR/run
URLs, In Progress = current state Sx, Next Steps). A fresh pod resumes by
reading it, then running the idempotency checks above.

## State machine

```dot
digraph harness {
  S1 -> S2 -> S3 -> S4 -> S5 -> S6 -> S7 -> S8 -> S9;
  S4 -> S3 [label="pipeline fail / unmergeable"];
  S6 -> S3 [label="main pipeline fail"];
  S7 -> S3 [label="diff wrong / unmergeable"];
  S8 -> S3 [label="apply fail -> rollback first"];
}
```

### S1 Research

1. `export GH_TOKEN="$GIT_TOKEN"`.
2. `gh issue view N --repo szymonrychu/<repo>` - read title, body, labels, comments.
3. Codebase context via tatara-mcp (the `query` + `code_*` tools): `query`
   (mode hybrid) for prose memory; `code_search` (repo=<repo>) to find entities;
   `code_explain` / `code_neighbors` / `code_callers` to map blast radius.
4. External research only if the issue needs it: WebSearch / WebFetch.
5. Do NOT comment yet.

### S2 Comment research

`gh issue comment N --repo szymonrychu/<repo> --body "<!-- harness:S2 -->
## Research + proposed approach
<summary, references, the approach you will implement>"`.

### S3 Implement (subagent-driven) - THE LOOP TARGET

This is where you return on any later failure. Re-running S3 means: address the
specific failure (red pipeline log, bad diff), re-review, re-push.

1. If the change needs design: REQUIRED SUB-SKILL: superpowers:brainstorming.
2. REQUIRED SUB-SKILL: superpowers:writing-plans for any multi-step change.
3. REQUIRED SUB-SKILL: superpowers:using-git-worktrees - isolate the component
   work in a worktree off the component repo's `main`.
4. REQUIRED SUB-SKILL: superpowers:subagent-driven-development with
   superpowers:test-driven-development - implement task-by-task, tests first.
5. REQUIRED SUB-SKILL: superpowers:requesting-code-review - fix every
   critical/high finding before proceeding.
6. `pre-commit run --all-files` in the component clone; fix until clean.
7. Post a progress comment under the issue: `gh issue comment N --body
   "<!-- harness:S3 --> implemented <scope>; opening MR"`.

### S4 Component MR + pipeline

1. `gh pr create --repo szymonrychu/<repo> --base main --head <branch>
   --title "<type: summary>" --body "Closes #N

<what + why>"`.
2. Watch checks: `gh pr checks <n> --repo szymonrychu/<repo> --watch
   --fail-fast`. (or `gh run watch <run-id>` for the triggered run.)
3. If checks fail OR `gh pr view <n> --json mergeable -q .mergeable` is
   `CONFLICTING`: comment the failure under the issue, then GO TO S3.

### S5 Self-merge

`gh pr merge <n> --repo szymonrychu/<repo> --merge --delete-branch`.

### S6 Watch main pipeline (image/chart build+push)

This is where the component CI builds + pushes the image/chart to harbor (do
NOT local buildx).

1. Find the post-merge run: `gh run list --repo szymonrychu/<repo> --branch main
   --limit 1 --json databaseId,headSha -q '.[0].databaseId'`.
2. `gh run watch <id> --repo szymonrychu/<repo> --exit-status`.
3. On failure: comment the failure, GO TO S3.
4. On success: record the new image tag / chart version (the build's pushed
   tag, e.g. the short SHA `0.0.0-<sha>` for tatara-operator). You need it in S7.

### S7 Helmfile MR

Work in the `tatara-helmfile` clone (`/workspace/szymonrychu/tatara-helmfile`).
REQUIRED SUB-SKILL: superpowers:using-git-worktrees - branch off `tatara-helmfile`
`main` for the bump.

1. Bump the release to the version S6 produced:
   - Image tag pin lives in `values/<release>/common.yaml` (e.g.
     `values/tatara-operator/common.yaml` carries `image.tag`). To bump it,
     REQUIRED SUB-SKILL: invoke `bump-container-usage` against the
     `tatara-helmfile` clone (it rewrites image references).
   - Chart version bumps: REQUIRED SUB-SKILL: invoke `bump-chart-usage`
     against the `tatara-helmfile` clone (it updates the release's chart version
     in `helmfile.yaml.gotmpl`).
   - If a skill does not fit the exact field, edit the value directly (KISS) and
     note why in the PR body.
2. `gh pr create --repo szymonrychu/tatara-helmfile --base main --head <branch>
   --title "deploy: <release> <version>" --body "Delivers issue
   szymonrychu/<repo>#N"`.
3. The `diff.yaml` workflow posts the `helmfile diff` as a sticky comment. Wait
   for it: `gh pr checks <n> --repo szymonrychu/tatara-helmfile --watch`, then
   read the sticky comment: `gh pr view <n> --repo szymonrychu/tatara-helmfile
   --json comments -q '.comments[].body'`.
4. Review the diff. If it changes anything other than the intended release
   bump, or the PR is unmergeable: GO TO S3 (the bump was wrong; fix the
   component or the values).

### S8 Merge helmfile MR + watch apply

1. `gh pr merge <n> --repo szymonrychu/tatara-helmfile --merge --delete-branch`.
   (Auto-apply: merge to main triggers `apply.yaml`.)
2. Find the apply run: `gh run list --repo szymonrychu/tatara-helmfile
   --workflow apply.yaml --branch main --limit 1 --json databaseId
   -q '.[0].databaseId'`.
3. `gh run watch <id> --repo szymonrychu/tatara-helmfile --exit-status`.
4. **On apply success:** GO TO S9.
5. **On apply failure - ROLLBACK FIRST, then S3:**
   a. In the `tatara-helmfile` clone on `main`: `git pull`, find the merge
      commit: `git log --merges -1 --format=%H`.
   b. `git revert -m 1 --no-edit <merge-sha>` (revert the merge, keep main's
      first parent).
   c. Push the revert on a branch and open + merge a revert PR so the SAME
      `apply.yaml` re-applies the prior good state:
      `git switch -c revert-<release>-<n> && git push -u origin HEAD`
      then `gh pr create --repo szymonrychu/tatara-helmfile --base main
      --head revert-<release>-<n> --title "revert: rollback <release> <version>"
      --body "Apply failed for #<n>; restoring prior state" &&
      gh pr merge <revert-n> --repo szymonrychu/tatara-helmfile --merge
      --delete-branch`.
   d. Watch the rollback apply run (same command as step 2-3) to confirm the
      cluster is restored.
   e. Comment the failure + rollback under the issue, then GO TO S3.

### S9 Deliver

1. `gh issue comment N --repo szymonrychu/<repo> --body "<!-- harness:S9 -->
## Delivered
- component MR: <url> (merged)
- helmfile MR: <url> (merged, applied)
- deployed: <release> <version>"`.
2. Record the outcome: call the tatara-mcp `issue_outcome` tool with
   `action: "implement"` and a short `comment` (it addresses the Task via
   `TATARA_TASK`). This is the authoritative success signal.
3. `gh issue close N --repo szymonrychu/<repo> --reason completed`.

## Deferred bugs / out-of-scope findings

If S1/S3 surface a separate bug or improvement that is out of this issue's
scope, do NOT silently expand scope. Call the tatara-mcp `propose_issue` tool
(`kind: bug|improvement`, `repo: <repo>`) so it lands behind awaiting-approval.

## Red flags - STOP

- About to merge a PR you have not watched go green -> watch first.
- About to declare delivered without `issue_outcome` -> not delivered.
- Apply failed and you jumped straight to S3 without rolling back -> roll back
  first (S8 step 5), the cluster is in a bad state.
- `docker buildx` / `docker push` locally -> never; images ship via component
  CI on merge (S6).
- Editing values in the component repo instead of `tatara-helmfile` -> the
  release lives in `tatara-helmfile`; component repo only builds the image.

## Quick reference

| State | Verb | Key command |
|-------|------|-------------|
| S1 | research | `gh issue view`; tatara-mcp `query`/`code_*` |
| S2 | comment | `gh issue comment` |
| S3 | implement | worktree + subagent-driven + TDD + code-review |
| S4 | component MR | `gh pr create`; `gh pr checks --watch` |
| S5 | merge | `gh pr merge --merge --delete-branch` |
| S6 | main pipeline | `gh run watch --exit-status` |
| S7 | helmfile MR | `bump-container-usage`/`bump-chart-usage`; `gh pr create` |
| S8 | apply | `gh pr merge`; `gh run watch`; revert on fail |
| S9 | deliver | `gh issue comment`; `issue_outcome`; `gh issue close` |
