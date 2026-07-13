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

| Method + path | Request | Response |
|---|---|---|
| `POST /v1/messages` | `{"text":"...","callbackUrl":"...","handoff":false}` | 202 `{"turnId":"..."}`; 409 turn in flight; **410 pod TTL expired** |
| `GET /v1/messages/{turnID}` | - | 200 `turn.Record` |
| `GET /v1/messages` | - | 200 `[turn.Summary]` |
| `GET /v1/session` | - | 200 `{... ,"contractVersion":2}` |
| `DELETE /v1/session` | - | 202, graceful shutdown, pod exits |
| `GET /v1/transcript` | - | 200, full JSONL session transcript (debug) |
| `GET /healthz` `/readyz` `/metrics` | - | Operator endpoints |

`POST /v1/interject` is gone (404) - mid-flight events now ride in at the
next turn boundary as the `<events>` block of the next bundle, not as
mid-turn PTY injection.

Turns are strictly sequential. `handoff:true` marks the operator's TTL stop
turn: it is the only turn admitted past `AGENT_POD_TTL_SECONDS` (exactly
once, bounded by `deadline + 2*turnTimeout + 60s`); before the deadline it
is inert and the turn is ordinary. `GET /v1/session`'s `contractVersion`
lets the operator assert the agent image and operator came from the same
release train before submitting turn 0. Result delivery: if `callbackUrl`
is set, the wrapper POSTs the turn result there on completion (retrying);
the result is always also retrievable by polling `GET /v1/messages/{turnId}`.

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
`AGENT_POD_TTL_SECONDS` (0, disabled - the operator sets this from
`Project.spec.agentPodTTLSeconds`), `BOOT_TIMEOUT_SECONDS` (60),
`WEBHOOK_RETRIES` (3).

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

### Lifecycle hooks

Operator-supplied shell commands (set on `Project.spec.agent.hooks`, delivered
as `HOOK_*` env vars) run at fixed points in the agent lifecycle. Each runs via
`sh -c <command>` in `/workspace` and is best-effort: a non-zero exit is logged
(WARN) and counted (`ccw_lifecycle_hook_total{result,hook}`) but never aborts
the run. An unset/empty `HOOK_*` is skipped.

| env var | fires | context passed |
| --- | --- | --- |
| `HOOK_PRE_CLONE` | before each repo clone | repo URL as `$1` + `TATARA_HOOK_REPO_URL` |
| `HOOK_POST_CLONE` | after each clone+checkout | clone dir as `$1` + `TATARA_HOOK_CLONE_DEST` |
| `HOOK_CONVERSATION_START` | once after the session boots | `TATARA_TASK`, `TATARA_PROJECT` |
| `HOOK_CONVERSATION_RESTART` | after a crash-relaunch that resumed (`--continue`) | `TATARA_TASK`, `TATARA_PROJECT` |
| `HOOK_AGENT_TURN_FINISHED` | after a turn is committed, pushed, and the callback delivered | `TATARA_TURN_ID` (+ task/project) |
| `HOOK_CONVERSATION_FINISHED` | once during shutdown (bounded to 5s) | `TATARA_TASK`, `TATARA_PROJECT` |

## Unattended boot

A fresh HOME would otherwise hit several interactive dialogs. The wrapper
suppresses onboarding, folder-trust, and custom-API-key prompts by seeding
`~/.claude.json`. The "Bypass Permissions mode" warning is NOT seedable and
appears on every boot; the wrapper detects it in the PTY ring buffer and
accepts it before accepting turns. See `docs/spike-findings.md`.

## Pod TTL and the handoff turn (replaces issue #114 S3 conversation persistence)

Every pod boots fresh: `internal/storage` (the S3 client) and the S3
restore/fork/upload path in `internal/convstore` were removed, along with the
chat-backed continuation preamble that used to prime a pod's first turn.
There is no cross-pod resume any more - every pod's turn-0 is the same
freshly rendered bundle.

Continuity across pods is instead a task-centric handoff bounded by the
pod's own lifetime:

- `AGENT_POD_TTL_SECONDS` (0 = disabled) is this pod's total lifetime
  budget, set by the operator from `Project.spec.agentPodTTLSeconds`. Past
  the deadline the wrapper refuses to start an ordinary turn (`410 pod ttl
  expired`) so a stale pod cannot silently keep working.
- Exactly one turn is admitted past the deadline: the operator's
  `"handoff": true` turn (bounded by `deadline + 2*turnTimeout + 60s`), used
  to have the agent write its continuation notes before the pod is torn
  down. `handoff:true` sent before the deadline is an ordinary turn and does
  not consume that allowance.
- REVIEW: an MR review pod sets `CHECKOUT_BRANCH` (the PR head) and no
  `TASK_BRANCH`, so it works on the PR code read-only and never pushes.
- `internal/convstore.TranscriptDir` is kept: it backs the boot-crash fix's
  on-disk transcript check (`session.shouldResume`), unrelated to S3 and
  unrelated to cross-pod continuity - this is intra-pod claude-process
  crash recovery only.

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
