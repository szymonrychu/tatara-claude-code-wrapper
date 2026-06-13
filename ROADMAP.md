# ROADMAP.md - tatara-claude-code-wrapper

Planned work not yet started. Per-platform roadmap lives in the parent repo.

## v0.1.0 (current)

Shipped: single-session interactive-claude supervisor over a PTY, OIDC-gated
HTTP API, webhook-or-poll turn delivery, bootstrap seeding (CLAUDE.md global+
project, merged MCP, skills, `~/.claude.json` no-dialog seed, settings), boot
dialog navigation, Stop-hook result capture, helm chart, modular Dockerfile.

- SHIPPED 2026-06-09 - SCM-projects MCP tools (propose_issue/review_verdict/pr_outcome) reach agents via the re-pinned baked tatara-cli 0.5.0; flow-through asserted by mcp_flowthrough_test.go.
- SHIPPED 2026-06-11 - autonomous-cron: issue_outcome MCP tool flows through after cli bump 0.5.0 -> 0.6.0; guard extended; chart 0.1.9/appVersion 0.1.8.
- SHIPPED 2026-06-11 - transcript streaming: Tailer+Redactor in internal/transcript; one agent_stream slog event per content block; ccw_stream_events_total counter; CCW_LOG_TRANSCRIPT=false disables; chart 0.1.13.
- SHIPPED 2026-06-13 - lifecycle notification params reach agents via re-pinned baked tatara-cli 0.6.0 -> 0.7.0 (change_summary gains mostProblematic, issue_outcome gains plan; cli also fixed change_summary's snake_case body keys to the operator's camelCase contract); chart 0.1.13 -> 0.1.14. mcp_flowthrough_test.go still passes unchanged (it asserts tool presence, not params; issue_outcome/pr_outcome remain). Pairs with tatara-operator 0.4.3 (issue#6).

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
- ~~**Webhook retries use `context.Background()`** and are not cancelled on
  shutdown.~~ Fixed: the sender owns a cancellable context and a `WaitGroup`;
  `app.shutdown` drains in-flight deliveries within a bounded window, then
  cancels retries (which log a clean abort) and joins the goroutines.
