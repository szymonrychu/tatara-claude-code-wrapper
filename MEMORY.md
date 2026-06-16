# MEMORY.md - tatara-claude-code-wrapper

- 2026-06-16 (review): OnTurnDone reverted from "deliver-then-async-push" to "push-then-deliver, both in a tracked background goroutine". The audit-r2 version inverted the order and detached the push fire-and-forget: that (1) let the operator's callback-triggered write-back read the task branch before the agent's commits were pushed, and (2) lost commits when the pod was torn down mid-push (goroutine untracked by shutdown). Fix: whole finalisation runs in app.turnWG-tracked goroutine (handler still returns fast, fixing the cc-stop-hook 5s budget), pushes first, delivers second; shutdown drains turnWG before the sender. Removed dead currentTimeoutStarted field (written, never read; timer is a relative AfterFunc). Submit write-failure now increments ccw_turns_total{failed} (was only turnsCompleted++, invisible in the metric). networkpolicy metrics-scrape rule was from-less on the public API port 8080 (= opened the control API cluster-wide); now gated on networkPolicy.monitoringNamespace, off by default.

- 2026-06-16 (audit-r2): resumeTurn sends a bare CR into the --continue session. Two unresolved hazards: (1) if claude already completed the turn before crashing, --continue restores the completed conversation and the bare CR submits an empty user turn, causing duplicate work; (2) if the paste landed but the submit keystroke was lost, no Stop hook fires for the original turn id and the turn sits Busy until TurnTimeout. These cannot be fixed without reading the last-message role from the restored transcript (tailer already parses it, but resumeTurn runs before any hook lands). Documented as a known limitation per hard rule 4; full fix requires transcript-aware resume (future work).

- 2026-06-16 (audit-r2): `.claude.json` customApiKeyResponses.approved stores last 20 chars of the Anthropic API key (partial secret at rest, 0600 file). Real sk-ant-* keys are always >20 chars so the suffix is never the full key. Intentional per the claude onboarding recipe; hardening would require a different auth method.

- 2026-06-16 (audit-r2): `.mcp.json` written 0644 (non-secret config). Overlay fragments must NOT embed secrets in their `env` map - use env passthrough (the pod's env provides secrets at runtime). Fragments with embedded credentials would be world-readable inside the pod. Single-tenant pod so impact is contained, but document as a usage constraint.

- 2026-06-15 (#26): Relaunch now calls `ring.reset()` between the dead and the relaunched claude. The 64KB ring is reused across relaunches, so a short/early-crashing turn could leave the prior boot's "Bypass Permissions mode" text resident; `bootWait` then false-matched it and fired Down+Enter into the still-initializing TUI before the real dialog drew, the unaccepted dialog ate the `resumeTurn` CR on its "No, exit" default, and claude exited again (intermittent restart-budget burn). `reset()` clears `buf` but leaves `total` monotonic so `bootWait`'s `written()` quiescence baseline survives the relaunch; called after `old.Close()` (old readPTY stopped) and before the new `readPTY`, so it cannot race the new proc's first bytes. Initial `Start` boot is unaffected (ring already empty).

- 2026-06-14: Push-metrics client (`internal/pushclient`, tatara-operator#42 B2). The wrapper Pod is too short-lived to be reliably pull-scraped, so it now PUSHES its Prometheus /metrics text to the operator's push-receiver (B1, `POST {OPERATOR_PUSH_URL}?run_id=&pod=&job=`). `pushclient.Pusher` gathers from the SAME registry that backs /metrics (`obs.PromRegistry`), encodes via `expfmt.NewEncoder(..., NewFormat(TypeTextPlain))`, and pushes on an interval (`PUSH_INTERVAL_SECONDS`, default 15s) starting with an immediate push so a fast-exiting run is not lost; on graceful shutdown it best-effort DELETEs its series (operator TTL is the hard-kill backstop). No-op unless the operator wired `OPERATOR_PUSH_URL` + `RUN_ID` (so local/CI runs don't push). Existing `ccw_*` metric names already avoid collision with the operator's `operator_*`/`tatara_*`, so no rename was needed. Wired in app.run()/app.shutdown() next to the webhook sender. Operator side sets OPERATOR_PUSH_URL (=callback base + /internal/metrics/push), RUN_ID and POD_NAME (both = wrapper Pod name) in BuildPod env.

- 2026-06-14: mise toolchain added (.mise.toml + CI mise-action). `mise run test` -> `make test` -> `go test -race` needs CGO_ENABLED=1 (race detector) which needs gcc; CI jobs (lint/test/build/smoke) each install build-essential (includes gcc + make) via apt BEFORE jdx/mise-action@v2. mise does NOT provide gcc; system build-essential is always required for race-enabled tests. min_version="2026.6.3" pinned in .mise.toml.

- 2026-06-14: Merge-integrated `Manager.handleExit` (from #16/tp786) alongside the interject feature: `watch()` now delegates to `handleExit`, which on an unexpected claude exit fails any in-flight turn (reusing failTimeout's bookkeeping), sets state Dead, and fires OnTurnDone so the caller learns immediately instead of hanging until the 30-minute timeout. `SimulateExitForTest` drives the path in tests. Both features kept; unrelated-histories merge of `tatara/task-task-d9rlf` with `main`.

- 2026-06-14: Added `POST /v1/interject` + `Manager.Interject(text)` for mid-session input: types paste+CR into the live claude turn (reusing the Submit keystroke sequence) WITHOUT creating a turn record or touching current/state/timer, so the running turn absorbs it and still completes with one Stop hook. `409`/`ErrNotBusy` when no turn is in flight. Counter `ccw_interjections_total`. Used by the operator to make issue/MR comments interrupt the agent nursing them (tatara-operator#25).

- 2026-06-13: tatara-deploy-harness skill is SKILL-ONLY (no wrapper Go/chart change). TATARA_REPOS is operator-built in pod.go BuildPod from the Project's Repository list; tatara-helmfile reaches the workspace via its self-enroll Repository CR, cloned by bootstrap.Render. Issue number comes from kickoff prompt + TATARA_TASK/TATARA_PROJECT env; gh authed by GIT_TOKEN.

- 2026-06-11 (0.1.13) - **Transcript streaming.** New `internal/transcript` package: Tailer (JSONL follow, inode-change reopen, 200ms poll) + Redactor (longest-first, >=8 char values, env key pattern match). One slog INFO `action=agent_stream` event per content block (text/thinking/tool_use/tool_result/message_end/raw). Counter `ccw_stream_events_total{stream_type}`. Wired via `Manager.StartTailer(ctx)` called in app.go before `sess.Start`; tailer goroutine starts on first `Complete()` that supplies a transcript path. Disabled by `CCW_LOG_TRANSCRIPT=false`. Spec: docs/superpowers/specs/2026-06-11-wrapper-transcript-streaming-design.md.

Cross-session decisions and hard-won findings. Newest first. One line each
unless the rationale is non-obvious. Per-platform decisions live in the parent
`tatara` repo's MEMORY.md.

---

- 2026-06-11 autonomous-cron: bumped baked cli 0.5.0 -> 0.6.0 for the issue_outcome MCP tool; tools are auto-discovered (RegisterTataraMCP wires `tatara mcp`, no enumeration), so the wrapper change is a version-bump + guard-extension only. mcp_flowthrough_test.go now also asserts issue_outcome; the Dockerfile test-guard stage enforces it at image build.
- 2026-06-10 - **SCM-projects MCP guard fires at IMAGE BUILD, not plain `make test`.** mcp_flowthrough_test.go t.Skip()s when tatara is absent from PATH, so `make test` on a host without the cli is harmless. The real enforcement is Dockerfile stage `test-guard` (golang:alpine + the tatara-cli binary copied onto PATH) which runs `go test ./internal/bootstrap -run TestTataraMCP_AdvertisesScmProjectTools -count=1`; the image build fails if propose_issue/review_verdict/pr_outcome are dropped. Also runs on any host/CI where tatara is on PATH.
- 2026-06-09 - **SCM-projects MCP tools flow through, not enumerated.** RegisterTataraMCP only runs `tatara mcp-config`, which wires `{command:tatara,args:[mcp]}`; agents see whatever `tatara mcp` serves (OperatorTools()). propose_issue/review_verdict/pr_outcome arrived for free once the baked cli was re-pinned 0.4.0 -> 0.5.0 (Dockerfile ARG + Makefile default). mcp_flowthrough_test.go runs the binary's MCP tools/list and asserts the 3 names so a future cli pin can't silently drop them. Chart 0.1.8 / appVersion 0.1.7.
- 2026-06-09 (0.1.6) - **Tatara MCP server registered at bootstrap.** After `bootstrap.Render` writes `.mcp.json`, wrapper runs `tatara mcp-config <workspace>` via an injected `CmdRunner` (`bootstrap.RegisterTataraMCP`). Gated on `TATARA_MEMORY_URL != ""` and `tatara` binary on PATH so dev/test without the CLI is unaffected. Non-fatal: `log.Error` then continues. `execRunner` passes `os.Environ()` so `tatara` finds its OIDC/memory env. Design: spec/2026-06-09-agent-native-tatara-tools-design.md A3.
- 2026-06-09 (0.1.5) - **Cross-repo agent support.** `TATARA_REPOS` (JSON array env) parsed into `[]bootstrap.RepoSpec`; `Render` clones each repo into `/workspace/<name>` and checks out `TASK_BRANCH` in each. `GitRunner` is now dir-parameterized (`func(dir string, args ...string) error`). `CommitAndPushAll` loops over repos on every `OnTurnDone`. Session config (`.mcp.json`, `.claude/`) stays in `/workspace` parent, outside all repos - retiring the `.git/info/exclude` hack. Primary repo clone failure is fatal; non-primary is best-effort (log + skip).
- 2026-06-09 (0.1.4) - Corrected build: the 0.1.3 IMAGE was built from main BEFORE
  the exclude+skills branch was merged (a merge slip), so it shipped neither the
  `.git/info/exclude` fix nor the superpowers skills - PRs still leaked scaffolding
  and the agent had no superpowers. 0.1.4 is the same intended change, built after
  the merge. Lesson: build/deploy ONLY after merging the feature branch to main
  (rule 10) - verify `git ls-files` shows the new files before `docker build`.
- 2026-06-08 (0.1.3) - **Clean PRs + superpowers on the agent.** (1) `git add -A`
  in the per-turn CommitAndPush swept the wrapper-injected `.mcp.json`/`.claude/`
  into the agent's PR. Fix: after clone, append those to the repo's
  `.git/info/exclude` (`internal/bootstrap/exclude.go`) so only the agent's real
  edits are committed. (2) Baked the full superpowers skill set into
  `templates/skills/` (alongside handoff) so spawned agents have brainstorming,
  writing-plans, TDD, systematic-debugging, requesting-code-review,
  verification, etc. Pairs with operator 0.2.7 (plan turn now lets the agent
  implement directly).
- 2026-06-08 (0.1.2) - enforce-push (see git log `feat(bootstrap): wrapper
  enforces branch checkout + commit/push per turn`).
- 2026-06-08 (0.1.1) - **Bootstrap now configures git creds + identity before
  clone.** The agent pod clones the target repo and the agent later pushes its
  branch, but bootstrap ran a bare `git clone` -> private repos failed
  `could not read Username for 'https://github.com'`. The operator already
  injects `GIT_TOKEN`; the wrapper ignored it. `configureGit` sets a global
  commit identity and a credential helper that reads `$GIT_TOKEN` at invocation
  (never written to disk), so clone AND the agent push authenticate. Surfaced on
  the first live operator->wrapper task (dogfood), the first real exercise of the
  spawned-agent path. Each git config is gated on a non-empty value.

- 2026-06-04 - **Drives the REAL interactive claude TUI over a PTY, never
  `-p`.** The whole point is that claude sees the same harness a human gets
  (skills, slash commands, normal hook/permission UX). `-p`/print mode is a
  divergent codepath and is forbidden. Input is typed into a PTY; output is
  captured via a custom Stop hook + the transcript, never parsed off the
  terminal.
- 2026-06-04 - **The "Bypass Permissions mode" warning dialog appears on
  EVERY boot and is NOT seedable.** It shows whenever bypass is active,
  including via `settings.defaultMode: bypassPermissions` (not just the
  `--dangerously-skip-permissions` flag). It is not persisted to
  `~/.claude.json`. `session.bootWait` detects it in the PTY ring buffer
  (whitespace-normalized match, because the TUI separates words with
  cursor-move escapes) and accepts it with Down+Enter before accepting turns.
  If unaccepted, the first turn's submit CR lands on the dialog (default
  "No, exit") and claude exits status 1. THE PTY MUST BE RING-BUFFERED, NOT
  DISCARDED - discarding it made this failure invisible for a while.
- 2026-06-04 - **Turn submit is TWO PTY writes, not one.** Bracketed-paste
  text, ~400ms pause (`SubmitDelay`), then CR. A single concatenated write
  (`paste+text+paste+\r`) leaves the text unsent in the input box.
- 2026-06-04 - **Readiness = output quiescence, not a fixed delay.** After
  accepting the bypass dialog, `bootWait` waits until PTY output is idle
  >1.5s (with a ~4s floor, capped by `BootTimeout`). claude renders its first
  frame in ~2s but is still doing background init; submitting that early
  kills it.
- 2026-06-04 - **`~/.claude.json` seed recipe for a no-dialog fresh HOME**
  (suppresses onboarding, folder-trust, custom-API-key dialogs; the bypass
  warning is handled separately at boot): `hasCompletedOnboarding:true`,
  `customApiKeyResponses.approved:["<LAST 20 CHARS of ANTHROPIC_API_KEY>"]`,
  `projects["/workspace"].hasTrustDialogAccepted:true`. The 20-char suffix is
  the exact key claude stores. Written by `bootstrap.writeClaudeJSON`.
- 2026-06-04 - **claude auth = long-lived `ANTHROPIC_API_KEY`** (wins the
  auth precedence over OAuth, no interactive login). Homelab source secret:
  `mtg/anthropic-api-key` key `ANTHROPIC_API_KEY` (also carries
  `CLAUDE_CODE_OAUTH_TOKEN`). Must be replicated into the `tatara` namespace
  for deployment.
- 2026-06-04 - **Whole turn loop validated end-to-end against real claude
  v2.1.162** with the long-lived key on a fresh HOME: bootstrap seed -> PTY
  boot + bypass-accept -> two-write submit -> real turn -> Stop hook -> internal
  endpoint -> store, returning `finalText:"PONG"` with full token usage.
- 2026-06-04 - **One session per pod; turns strictly sequential (409 while
  busy).** No multiplexing, no session registry. K8s/argo owns which-pod and
  keep-alive. Webhook-or-poll delivery; `callbackUrl` optional.
- 2026-06-04 - **claude in its own Docker layer behind `ARG
  CLAUDE_CODE_VERSION`** so bumping it (`make bump-claude VERSION_ARG=...`)
  rebuilds only that layer, never the Go binaries or overlay config.
- 2026-06-04 - **ConfigMap env keys are UPPER_SNAKE consumed via envFrom**
  (matches tatara-memory). List/multiline/map config (CLAUDE.md bodies, MCP
  overlays, skills, allowed-tools) goes into a separate ConfigMap mounted at
  `/etc/wrapper` and read as files at runtime, per hard rule 6.
- 2026-06-04 - **`.dockerignore` must re-include `templates/`** (`!templates/**`
  after the `*.md` glob) or `templates/skills/handoff/SKILL.md` is excluded
  from the build context and `COPY templates/` copies an empty dir.

## Dead-ends / things tried that did not work

- 2026-06-04 - **`--dangerously-skip-permissions` to skip boot dialogs.** It
  does NOT skip folder-trust or the custom-API-key prompt, and it ADDS the
  bypass warning. Use settings-based bypass + `~/.claude.json` seeding +
  boot-time navigation of the (unavoidable) bypass warning instead.
- 2026-06-04 - **Single concatenated submit write.** Does not submit; needs
  two writes with a pause.
- 2026-06-04 - **Fixed short boot delay for readiness.** Too early; input
  during background init exits claude. Use output quiescence.

- 2026-06-07 - **First image push (0.1.0)** built with `--build-arg TATARA_CLI_VERSION=0.4.0`. The tatara-cli base image must be pushed first (wrapper `COPY --from=tatara-cli` pulls from Harbor). GO_VERSION=1.25 matched wrapper go.mod `go 1.25.0` without bumping. claude@latest resolved to a 2-package install (no issues); claude binary lands at `/usr/local/bin/claude`. All four binaries confirmed present: tatara, claude, wrapper, git.

## Open questions

- 2026-06-04 - **claude crash = lost context (v0.1.0).** Transcript is
  persisted and `/workspace` is PVC-able, so `claude --resume` is a clean v0.2
  follow-up.

2026-06-14 **watch() never restarted claude -> added detect+resume.** session.go watch() detected claude-code process death (cmd.Wait returns) but only set state=Dead: it never failed the in-flight turn and never relaunched claude (the ClaudeRestarts metric was named for a restart that was never implemented). Consequence: a mid-turn claude crash hung the turn until the 1800s failTimeout and left a permanently-Dead zombie pod (wrapper HTTP servers kept running, Submit rejected everything) - the operator only recovered via turn-timeout + pod respawn. Fix: on unexpected exit, relaunch claude with `--continue` (restores the cwd conversation), re-submit the in-flight turn (same turn id + original timer so the Stop-hook still correlates and the operator deadline still bounds it), bounded to MaxRestarts=3 consecutive crash-relaunches; over the cap -> fail the turn immediately (no 30min wait) + state=Dead so the operator respawns. Counter resets on a successful Complete. Test seam: claudeProcess interface (pty.go) + injectable Manager.spawn; tests drive a fake whose Wait() the test controls and run under -race. shouldResume() returns false only on a first-boot death (no turn ever submitted / none completed / no transcript) so --continue is never used with no conversation. Review fixes: (1) relaunch Close()s the dead proc's PTY master (fd leak, one per crash, NOT bounded by the cap); (2) bootWait takes the proc and bails when superseded (isActiveProc) so a death-during-boot does not leave a stale bootWait polling the ring for the full BootTimeout. Branch feat/claude-death-resume; design+plan in tatara repo docs/superpowers/plans/2026-06-14-wrapper-claude-death-resume.md. NOT handled here (operator-side): a wrapper that crashes on BOOT (exits 1, readyz never up) still wedges the Task in Planning-without-turn - see tatara-operator#44.
