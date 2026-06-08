# MEMORY.md - tatara-claude-code-wrapper

Cross-session decisions and hard-won findings. Newest first. One line each
unless the rationale is non-obvious. Per-platform decisions live in the parent
`tatara` repo's MEMORY.md.

---

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
