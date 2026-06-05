# How tatara-claude-code-wrapper works

This is the operator/developer reference for the component. It describes the
real implemented behavior, not aspirations. For the raw empirical findings
about how the `claude` binary behaves (captured against v2.1.162), see
`spike-findings.md`. For why decisions were made, see `../MEMORY.md`.

---

## 1. One sentence

A Go service (PID 1 in the pod) spawns a single **interactive** `claude`
process attached to a pseudo-terminal, types one user turn at a time into it,
and captures each turn's result via a Claude Code **Stop hook** that POSTs
back to a loopback endpoint - exposing the whole thing as an OIDC-gated HTTP
API with webhook-or-poll delivery.

## 2. Why interactive-over-PTY and not `claude -p`

`claude -p` (print/headless mode) is a different codepath from the interactive
TUI: different system prompt assembly, different skill/hook/permission
behavior. The whole reason this component exists is to run the agent in the
**same harness a human gets**. So the wrapper allocates a PTY
(`github.com/creack/pty`), runs interactive `claude` as if a terminal were
attached, and "types" messages in. It never parses the terminal for results -
the terminal stream is only used to detect boot dialogs and for debug logging.
Results come from the Stop hook plus the on-disk transcript.

`-p` and `--dangerously-skip-permissions` are forbidden (the latter does not
skip boot dialogs and adds an extra one - see boot sequence below).

## 3. Component map

```
cmd/wrapper/         main service (PID 1): config -> bootstrap -> session -> HTTP
  config.go            env (+ a couple flags) -> typed config
  app.go               wiring: bootstrap.Render, session.New+Start, webhook, httpapi, 2 servers
  main.go              signal handling + graceful shutdown
cmd/cc-stop-hook/    the Stop-hook binary claude runs at end-of-turn
  main.go              read stdin payload, POST result to loopback, ALWAYS exit 0
  hook.go              build the result (prefers payload.last_assistant_message)
  transcript.go        parse the JSONL transcript for usage / text fallback
internal/session/    THE CORE: supervises the claude process + turn state machine
  pty.go               spawn claude under a PTY (ptyWriter seam for tests)
  ring.go              thread-safe ring buffer of PTY output (+ ANSI/whitespace-normalized match)
  session.go           boot navigation, submit, complete, timeout, snapshot
internal/bootstrap/  renders every file claude reads at startup
  bootstrap.go         orchestrates: repo clone, CLAUDE.md, MCP, settings, claude.json, skills
  claudejson.go        seeds ~/.claude.json for a no-dialog fresh-HOME boot
  settings.go          ~/.claude/settings.json (Stop hook + bypass mode + MCP auto-enable)
  mcp.go               merge base + overlay MCP fragments -> /workspace/.mcp.json
  skills.go            copy baked + custom skills into /workspace/.claude/skills
  repo.go              optional shallow git clone into /workspace
internal/httpapi/    chi router: OIDC public surface + loopback internal endpoint
internal/turn/       turn record + thread-safe in-memory store
internal/webhook/    async, retrying delivery of a turn result to a callback URL
internal/metrics/    ccw_* prometheus collectors
internal/auth/        OIDC JWT verifier + chi middleware (copied from tatara-memory)
internal/obs/         slog JSON logger + prometheus registry (copied)
```

The session package is the only stateful, concurrent part. Everything else is
either stateless (httpapi, webhook, bootstrap) or a plain store (turn).

## 4. Boot sequence

`cmd/wrapper` runs once at pod start:

1. **Load config** from env (scalars) and mounted ConfigMap files (paths).
2. **`bootstrap.Render`** writes, in order:
   - clone `REPO_URL@REPO_BRANCH` into `/workspace` if set (else empty scratch);
   - `/workspace/CLAUDE.md` (from `projectClaudeMd`), `~/.claude/CLAUDE.md`
     (from `globalClaudeMd`);
   - `/workspace/.mcp.json` = baked tatara-cli memory server merged with any
     `/etc/wrapper/mcp.d/*.json` overlays (0600);
   - `~/.claude/settings.json`: the Stop hook -> `/usr/local/bin/cc-stop-hook`,
     `permissions.defaultMode: bypassPermissions`, `enableAllProjectMcpServers: true`;
   - `~/.claude.json` (0600): the no-dialog seed - `hasCompletedOnboarding`,
     `customApiKeyResponses.approved: ["<last 20 chars of ANTHROPIC_API_KEY>"]`,
     `projects["/workspace"].hasTrustDialogAccepted: true`;
   - skills: baked (`/templates/skills`) + custom (`/etc/wrapper/skills`) copied
     into `/workspace/.claude/skills`.
3. **`session.Start`** spawns interactive `claude` under a PTY (no permission
   flag), starts a goroutine reading the PTY into the ring buffer, a goroutine
   `Wait`-ing on the process, and then runs `bootWait`.
4. **`bootWait`** (the critical bit):
   - The seed suppresses onboarding / folder-trust / custom-API-key dialogs.
   - The **"Bypass Permissions mode" warning is NOT seedable** and appears on
     every boot. `bootWait` detects it in the ring buffer (matching with ANSI
     and whitespace stripped, since the TUI lays out words with cursor-move
     escapes) and accepts it by sending Down + Enter ("Yes, I accept").
   - It then waits for **output quiescence** (no new PTY bytes for >1.5s, with
     a ~4s floor, capped by `BOOT_TIMEOUT_SECONDS`) and marks the session
     `ready`. A fixed short delay is wrong: claude renders its first frame in
     ~2s but is still initializing; submitting that early exits it.
5. `cmd/wrapper` then starts the two HTTP servers. Until `Start` returns,
   `/readyz` is not served, so the K8s readiness probe simply retries.

If `claude` exits for any reason other than a deliberate `Shutdown`, the
session goes `dead`, `/readyz` fails, the pod is restarted by Kubernetes, and
`ccw_claude_restarts_total` increments. In-RAM turn history is lost (the
transcript on disk survives if `/workspace` is a PVC).

## 5. Turn lifecycle

State machine: `booting -> ready -> busy -> ready -> ... -> dead`.

```
POST /v1/messages {text, callbackUrl?}
  httpapi.postMessage -> session.Submit:
    if state != ready (busy/booting/dead) -> 409 / error
    create turn record (state=running), id = "turn-<base36 nanos>"
    WRITE 1 to PTY: ESC[200~ <text> ESC[201~        (bracketed paste)
    sleep SubmitDelay (~400ms)                       <-- one write does NOT submit
    WRITE 2 to PTY: \r                               (carriage return submits)
    state = busy; start TurnTimeout timer
  -> 202 {turnId}

...claude runs the turn: reads CLAUDE.md, calls MCP tools, edits /workspace...

end of turn -> claude runs the Stop hook (cc-stop-hook):
  reads the hook payload on its stdin: {session_id, transcript_path,
    last_assistant_message, ...}
  builds HookResult: FinalText = last_assistant_message (authoritative),
    Usage + text-fallback from the transcript's last assistant line,
    ResultJSON from /workspace/result.json if the agent wrote one,
    TranscriptPath from the payload
  POSTs it to http://127.0.0.1:<INTERNAL_ADDR>/internal/turn-complete
  ALWAYS exits 0 (a hook must never block or alter claude)

httpapi.turnComplete -> session.Complete:
  stop the timeout timer
  record transcriptPath (for GET /v1/transcript)
  store.Complete(turnId, finalText, resultJson, usage, ...)
  state = ready; metrics (turns_total, turn_duration, hook_received)
  fire OnTurnDone(record) OUTSIDE the lock

OnTurnDone (wired in app.go) -> webhook.Deliver:
  if record.CallbackURL (or DEFAULT_CALLBACK_URL) set, POST the record there,
  retrying with exponential backoff up to WEBHOOK_RETRIES, else drop + metric.
  The result is ALWAYS also retrievable via GET /v1/messages/{turnId}.
```

Turns are strictly sequential - one `claude` process handles one message at a
time, so at most one turn is in flight and a second `POST /v1/messages` while
busy returns `409`. The caller waits for the result (webhook or poll) before
the next turn.

**Timeout**: if no Stop-hook callback arrives within `TURN_TIMEOUT_SECONDS`
(default 1800), the turn is marked `failed`, the callback (if any) is fired
with the failure, and pollers see `state=failed`. The timer/complete race is
guarded: whichever fires first wins; the other is a no-op because it checks the
current turn id.

## 6. HTTP API

Public surface (`Router()`), all `/v1/*` require a valid Keycloak JWT
(`aud` contains `tatara-claude-code-wrapper`):

| Method | Path | Notes |
|---|---|---|
| POST | `/v1/messages` | `{text, callbackUrl?}` -> `202 {turnId}` or `409` if busy / `400` if no text |
| GET | `/v1/messages` | history: `[{turnId, state, startedAt, completedAt}]` |
| GET | `/v1/messages/{turnId}` | full result; `404` if unknown |
| GET | `/v1/session` | `{state, turnsCompleted, model, repo}` |
| GET | `/v1/transcript` | full JSONL transcript; `404` before the first turn |
| DELETE | `/v1/session` | graceful shutdown -> pod exits |

Operator surface (open, not ingress-exposed): `/healthz` (always 200 while the
process lives), `/readyz` (200 once the session is alive, 503 otherwise),
`/metrics`.

Internal surface (`InternalRouter()`, bound to `127.0.0.1` only, no OIDC):
`POST /internal/turn-complete` - the Stop hook's target. It is on a separate
listener/port so it is never reachable from outside the pod.

OIDC is mandatory in deployment: the public router only skips the auth
middleware when no verifier is configured, which happens only when
`OIDC_ISSUER` is empty (tests / explicit local runs). The chart always sets it.

## 7. Configuration

Per platform hard rule 6, scalars arrive as UPPER_SNAKE env vars (the chart's
ConfigMap consumed via `envFrom`); list/multiline/map config is mounted as
files under `/etc/wrapper` and read at runtime.

Scalar env (defaults in parentheses): `HTTP_ADDR` (`:8080`), `INTERNAL_ADDR`
(`127.0.0.1:8090`), `OIDC_ISSUER` (Keycloak master realm), `OIDC_AUDIENCE`
(`tatara-claude-code-wrapper`), `LOG_LEVEL` (`info`), `MODEL` (claude default),
`PERMISSION_MODE` (`bypassPermissions`), `REPO_URL`/`REPO_BRANCH` (empty),
`DEFAULT_CALLBACK_URL` (empty), `TURN_TIMEOUT_SECONDS` (1800),
`BOOT_TIMEOUT_SECONDS` (60), `WEBHOOK_RETRIES` (3), plus the `*_PATH` /
`*_DIR` pointers into `/etc/wrapper` and `/templates/skills`.

Mounted files (chart values -> file): `globalClaudeMd`, `projectClaudeMd`,
`baseMcp`, `extraMcpServers` (map -> `mcp.d/<name>.json`), `allowedTools`
(list -> `allowed-tools.txt`), custom `skills`.

Secrets: `ANTHROPIC_API_KEY` (the long-lived key claude authenticates with -
injected via `secretKeyRef`), tatara-memory client credentials for the bundled
MCP, and git credentials if cloning a private repo.

## 8. Observability

JSON logs (`slog`) for every state transition and business action with
`turn_id` / `duration_ms`. Prometheus metrics on `/metrics`:

- `ccw_turns_total{result="complete|failed"}` - counter
- `ccw_turn_duration_seconds` - histogram
- `ccw_turn_in_flight` - gauge (0 or 1)
- `ccw_claude_restarts_total` - counter (claude process exits, excluding clean shutdown)
- `ccw_webhook_delivery_total{result="ok|dropped"}` - counter
- `ccw_hook_received_total` - counter

When claude exits unexpectedly, the last ~800 bytes of de-ANSI'd PTY output are
logged as `pty_tail` - the single most useful field for diagnosing a boot or
dialog regression.

## 9. Deployment (reference, not yet executed)

The chart is `charts/tatara-claude-code-wrapper` (helm-unittest covered).
Bringing it up requires, outside this repo:

1. **Keycloak client** `tatara-claude-code-wrapper` (confidential or the
   audience mapper) in the master realm at `auth.szymonrichert.pl` - terraform
   alongside the existing `tatara-memory` / `tatara-cli` clients in
   `infra/terraform/keycloak`.
2. **`ANTHROPIC_API_KEY` secret in the `tatara` namespace.** The homelab source
   is `mtg/anthropic-api-key` (key `ANTHROPIC_API_KEY`); replicate it (sops or a
   copy) into `tatara`, and set the chart's `anthropicApiKeySecret` /
   `anthropicApiKeyKey` to match.
3. **CI**: register the repo in the `infra` argo-events registry (push_tag ->
   go-ci + container-build + helm-publish) and add the GitHub webhook, mirroring
   tatara-memory / tatara-cli.
4. **Tag `v0.1.0`** to trigger the build, then add the helm release in
   `infra/helmfile` (tatara namespace) - or in `tatara-helmfile` once the
   consolidation P14 bump-and-deploy loop is live.

ClusterIP only for v0.1.0; the only caller is in-cluster (argo or a future
orchestrator). One session per pod - "which pod / keep it alive / route the
next turn" is the orchestrator's job, deliberately out of scope here.

## 10. Local development

```
make test                       # unit tests, -race
make build                      # bin/wrapper + bin/cc-stop-hook
go test ./cmd/wrapper -tags integration   # full turn loop with a stub claude (no API key)
make chart-test                 # helm unittest
make bump-claude VERSION_ARG=1.2.3        # bump bundled claude (its own Docker layer)
```

To smoke against **real** claude locally (needs the claude CLI + a real
`ANTHROPIC_API_KEY`), run `bin/wrapper` with `OIDC_ISSUER=""` (skips auth),
`HOME_DIR`/`WORKSPACE` pointing at fresh non-symlinked dirs (so claude's
resolved cwd matches the `/workspace` trust seed - on macOS use `/private/tmp/...`,
not `/tmp/...`), `HOOK_PATH` set to the built `cc-stop-hook`, and
`CCW_INTERNAL_URL` matching `INTERNAL_ADDR`. Then `POST /v1/messages` and poll
`GET /v1/messages/{turnId}`. This exercises the exact production path
(bootstrap seed, PTY boot + bypass-accept, two-write submit, hook callback).
```
