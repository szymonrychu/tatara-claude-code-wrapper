# ROADMAP.md - tatara-claude-code-wrapper

Planned work not yet started. Per-platform roadmap lives in the parent repo.

## v0.1.0 (current)

Shipped: single-session interactive-claude supervisor over a PTY, OIDC-gated
HTTP API, webhook-or-poll turn delivery, bootstrap seeding (CLAUDE.md global+
project, merged MCP, skills, `~/.claude.json` no-dialog seed, settings), boot
dialog navigation, Stop-hook result capture, helm chart, modular Dockerfile.

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
