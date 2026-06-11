# ROADMAP.md - tatara-claude-code-wrapper

Planned work not yet started. Per-platform roadmap lives in the parent repo.

## v0.1.0 (current)

Shipped: single-session interactive-claude supervisor over a PTY, OIDC-gated
HTTP API, webhook-or-poll turn delivery, bootstrap seeding (CLAUDE.md global+
project, merged MCP, skills, `~/.claude.json` no-dialog seed, settings), boot
dialog navigation, Stop-hook result capture, helm chart, modular Dockerfile.

- SHIPPED 2026-06-09 - SCM-projects MCP tools (propose_issue/review_verdict/pr_outcome) reach agents via the re-pinned baked tatara-cli 0.5.0; flow-through asserted by mcp_flowthrough_test.go.
- SHIPPED 2026-06-11 - autonomous-cron: issue_outcome MCP tool flows through after cli bump 0.5.0 -> 0.6.0; guard extended; chart 0.1.9/appVersion 0.1.8.

## v0.2 candidates

- **`claude --resume` after crash.** On claude exit, restart and resume from
  the persisted transcript instead of losing context. Needs `/workspace` on a
  PVC (chart toggle already exists).
- **Turn queueing** instead of 409-on-busy (optional, opt-in).
- **Tighter readiness signal.** Detect the input-box-ready marker in the PTY
  stream rather than relying on output quiescence.
- **Defense-in-depth boot navigation.** Have `bootWait` also handle the
  folder-trust and custom-API-key dialogs (currently suppressed by the
  `~/.claude.json` seed) in case seeding drifts across claude versions.
- **Ingress + multi-pod router/controller.** v0.1.0 is ClusterIP, one session
  per pod; external routing and pod lifecycle orchestration are out of scope.
- **SSE live streaming** of in-progress turn output (currently result-only).
- **Bedrock / Vertex auth** passthrough (currently `ANTHROPIC_API_KEY` only).
- **PTY ring-buffer debug endpoint** (expose `tail` over the API for live
  troubleshooting; currently only logged on claude exit).

## Known hardening (deferred from v0.1.0 code review, low impact)

- **Submit holds the lock during the ~400ms SubmitDelay** (`session.Submit`).
  Fine for single-session sequential turns, but it briefly blocks `readyz`/
  `Snapshot`/`Shutdown`. Decouple with a `submitting` guard if it ever matters.
- **Webhook retries use `context.Background()`** and are not cancelled on
  shutdown (`app.go` OnTurnDone). Best-effort with poll fallback, so a dropped
  delivery is recoverable; thread an app-owned cancellable context for a clean
  shutdown.
