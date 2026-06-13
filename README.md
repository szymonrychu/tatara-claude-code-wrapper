# tatara-claude-code-wrapper

A single-session Claude Code supervisor. One pod runs one persistent
**interactive** `claude` process driven over a PTY (never `-p`), submits one
user turn at a time, captures each turn's result via a custom Stop hook, and
exposes it through an OIDC-gated HTTP API with webhook-or-poll delivery.

Part of the [tatara](https://github.com/szymonrychu/tatara) platform (phase 4).

**Docs:** [`docs/architecture.md`](docs/architecture.md) - full how-it-works
(boot sequence, turn lifecycle, state machine, config, observability,
deployment, local dev). [`docs/spike-findings.md`](docs/spike-findings.md) -
the empirical claude-binary behavior the design rests on. [`MEMORY.md`](MEMORY.md)
- decisions and dead-ends.

## Why a PTY, not `-p`

The point is that claude runs in its **real interactive harness** - the same
codepath a human gets: skills, slash commands, normal hook and permission UX.
`-p`/print mode is a divergent codepath and is deliberately avoided. The
wrapper allocates a PTY, spawns interactive `claude`, types each message in
(bracketed paste + submit), and reads results from the Stop hook + transcript.
Terminal output is never parsed for results - it is ring-buffered only for
boot-dialog detection and debug logging.

## API

All `/v1/*` require an OIDC bearer token (Keycloak master realm, audience
`tatara-claude-code-wrapper`). Operator endpoints are open and not exposed via
ingress.

| Method | Path | Description |
|---|---|---|
| POST | `/v1/messages` | Submit a turn `{text, callbackUrl?}` -> `202 {turnId}` (or `409` if a turn is in flight). |
| GET | `/v1/messages` | Turn history `[{turnId, state, startedAt, completedAt}]`. |
| GET | `/v1/messages/{turnId}` | Full turn result `{state, finalText, resultJson?, usage, stopReason, error?}` (poll / missed-callback path). |
| GET | `/v1/session` | `{state, turnsCompleted, model, repo}`. |
| GET | `/v1/transcript` | Full JSONL session transcript (debug). |
| GET | `/v1/pty` | De-ANSI'd tail of the PTY ring buffer for live boot/wedge troubleshooting; `?bytes=N` (default 4096, capped at 64 KiB) (debug). |
| DELETE | `/v1/session` | Graceful shutdown, pod exits. |
| GET | `/healthz` `/readyz` `/metrics` | Operator endpoints. |

Turns are strictly sequential. Result delivery: if `callbackUrl` is set, the
wrapper POSTs the turn result there on completion (retrying); the result is
always also retrievable by polling `GET /v1/messages/{turnId}`.

## How a turn flows

```
POST /v1/messages -> wrapper types the message into the PTY -> 202 {turnId}
claude works (MCP, edits in /workspace) -> end of turn -> cc-stop-hook runs
  hook reads last_assistant_message + transcript -> POSTs to loopback internal endpoint
wrapper records the result -> POSTs to callbackUrl (if any); pollable via GET
```

## Configuration

Scalars come in as env (UPPER_SNAKE, via the chart's ConfigMap `envFrom`):
`HTTP_ADDR`, `INTERNAL_ADDR`, `OIDC_ISSUER`, `OIDC_AUDIENCE`, `LOG_LEVEL`,
`MODEL`, `PERMISSION_MODE` (default `bypassPermissions`), `REPO_URL`,
`REPO_BRANCH`, `DEFAULT_CALLBACK_URL`, `TURN_TIMEOUT_SECONDS` (1800),
`BOOT_TIMEOUT_SECONDS` (60), `WEBHOOK_RETRIES` (3).

File/list/multiline config is mounted under `/etc/wrapper` (chart values
`globalClaudeMd`, `projectClaudeMd`, `baseMcp`, `extraMcpServers`,
`allowedTools`, custom skills) and read at runtime. Secrets:
`ANTHROPIC_API_KEY` (the long-lived key claude authenticates with), plus
tatara-memory client credentials for the bundled MCP and git creds for a
private repo clone.

At boot the wrapper renders `~/.claude/CLAUDE.md`, `/workspace/CLAUDE.md`,
`/workspace/.mcp.json` (tatara-cli memory server + overlays),
`~/.claude/settings.json` (Stop hook + bypass mode + MCP auto-enable),
installs skills, optionally clones a repo, and seeds `~/.claude.json` so a
fresh HOME boots with no interactive dialogs.

## Unattended boot

A fresh HOME would otherwise hit several interactive dialogs. The wrapper
suppresses onboarding, folder-trust, and custom-API-key prompts by seeding
`~/.claude.json`. The "Bypass Permissions mode" warning is NOT seedable and
appears on every boot; the wrapper detects it in the PTY ring buffer and
accepts it before accepting turns. See `docs/spike-findings.md`.

## Build

```
make build                      # wrapper + cc-stop-hook
make test                       # go test -race
make image                      # multi-stage container
make bump-claude VERSION_ARG=1.2.3   # bump the bundled claude (its own layer)
make chart-test                 # helm unittest
```

claude lives in its own Docker layer behind `ARG CLAUDE_CODE_VERSION`, so
bumping it rebuilds only that layer.

## v0.1.0 limitations

- claude crash loses in-RAM context (pod restarts; `--resume` is v0.2).
- 409-on-busy, no turn queueing.
- ClusterIP only, one session per pod (no ingress / multi-pod router).
- No live streaming (result-only).
- `ANTHROPIC_API_KEY` auth only (no Bedrock/Vertex).
