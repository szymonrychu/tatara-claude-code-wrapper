# Spike findings - interactive PTY driving + Stop hook (Task 1)

Captured 2026-06-04 against `claude` **v2.1.162** on darwin, driving the real
interactive TUI over a PTY (`github.com/creack/pty`) in a fresh `$HOME`.
These supersede any guesses in the design/plan. Downstream tasks (7, 8, 6,
12, 13) MUST match these.

## 1. Persistent multi-turn over PTY: CONFIRMED

One long-lived interactive `claude` process handled multiple sequential user
turns (PONG, then PING), each firing its own Stop hook, with no respawn.
`-p`/`--print` was never used. The architecture holds.

## 2. Submit sequence: CONFIRMED (TWO writes, not one)

To submit one user turn, write to the PTY master as **two separate writes**:

```
write 1:  ESC[200~  <message text>  ESC[201~     (bracketed paste)
<~400ms pause>
write 2:  \r                                     (carriage return submits)
```

`session.DefaultSubmitSeq{PasteStart:"\x1b[200~", PasteEnd:"\x1b[201~", Submit:"\r"}`,
written as two calls with `SubmitDelay` (default 400ms) between them. A SINGLE
concatenated write (`paste+text+paste+\r`) does NOT submit - claude leaves the
text in the input box unsent. Bracketed paste keeps embedded newlines from
submitting early.

## 2b. Readiness: output quiescence after accepting the bypass dialog

`bootWait` marks READY when, after handling the bypass warning, PTY output has
been idle for >1.5s (with a ~4s floor, capped by `BootTimeout`). A fixed short
delay is wrong: claude renders its first frame in ~2s but is still doing
background init; submitting that early kills it.

## 3. Boot dialogs: the critical finding

A fresh `$HOME` + `ANTHROPIC_API_KEY` shows up to THREE blocking interactive
dialogs before the prompt. `--dangerously-skip-permissions` does NOT skip them
(it ADDS the third). The clean fix is to **pre-seed `~/.claude.json` and use
settings-based bypass, passing NO permission flag**. With seeding in place,
ZERO dialogs appear and turns run autonomously.

Dialogs and their seed keys (all in `~/.claude.json`):

| Dialog | Trigger | Seed to suppress |
|---|---|---|
| First-run onboarding (theme/login) | empty `~/.claude.json` | `"hasCompletedOnboarding": true` |
| Folder trust ("trust this folder?") | new cwd | `projects["<cwd>"].hasTrustDialogAccepted: true` |
| Custom API key ("use this API key?") | `ANTHROPIC_API_KEY` set | `customApiKeyResponses.approved: ["<last 20 chars of key>"]` |
| Bypass Permissions WARNING | `bypassPermissions` mode (settings OR flag) | NOT seedable - appears on EVERY boot. The wrapper accepts it at boot over the PTY (see "Correction" below). |

> **Correction (validated during implementation, supersedes an earlier
> claim):** the "Bypass Permissions mode" warning appears on EVERY boot when
> `bypassPermissions` is active, including via `settings.defaultMode` (not just
> the `--dangerously-skip-permissions` flag). It is not persisted to
> `~/.claude.json` and cannot be seeded away. The wrapper's `bootWait` detects
> it in the PTY ring buffer (whitespace-normalized match, since the TUI
> separates words with cursor-move escapes) and accepts it with Down+Enter
> ("Yes, I accept"). If unaccepted, the first turn's submit CR lands on the
> dialog (default "No, exit") and claude exits status 1. The PTY MUST be
> ring-buffered, not discarded, or this is invisible.

The custom-API-key approved entry is the **last 20 characters of the API key**
verbatim (e.g. key ending `...EentiTPHC9Q-62Rz1wAA` -> approved entry
`"EentiTPHC9Q-62Rz1wAA"`).

The cwd project key uses the RESOLVED absolute path. In the container the cwd
is `/workspace`, so seed `projects["/workspace"].hasTrustDialogAccepted: true`.

### Production launch recipe (no dialogs, autonomous)

1. Pre-seed `~/.claude.json`:
   ```json
   {
     "hasCompletedOnboarding": true,
     "autoUpdates": true,
     "customApiKeyResponses": { "approved": ["<LAST20_OF_ANTHROPIC_API_KEY>"], "rejected": [] },
     "projects": { "/workspace": { "hasTrustDialogAccepted": true } }
   }
   ```
2. `~/.claude/settings.json`: `"permissions": { "defaultMode": "bypassPermissions" }`,
   plus `enableAllProjectMcpServers: true` and the Stop hook.
3. Launch `claude` with **no** `--permission-mode` / `--dangerously-skip-permissions`
   flag. (`--model <m>` is fine.) MCP comes from the cwd `.mcp.json` +
   `enableAllProjectMcpServers`.

> Design impact: `bootstrap` gains a `~/.claude.json` writer (`claudejson.go`)
> that computes the last-20 key suffix and the `/workspace` trust entry.
> `session.claudeArgs()` must NOT add a permission flag.

## 4. Stop hook payload (real schema, v2.1.162)

```json
{
  "session_id": "b73e0299-...",
  "transcript_path": "<home>/.claude/projects/-private-tmp-ccw-spike-work/<session_id>.jsonl",
  "cwd": "/private/tmp/ccw-spike/work",
  "permission_mode": "bypassPermissions",
  "effort": { "level": "high" },
  "hook_event_name": "Stop",
  "stop_hook_active": false,
  "last_assistant_message": "PONG",
  "background_tasks": [],
  "session_crons": []
}
```

Key points:
- **`last_assistant_message` is present** - the final assistant text is handed
  to the hook directly. `cc-stop-hook` uses this for `FinalText`; it does NOT
  need to parse the transcript for the text. (Big simplification to Task 8.)
- **There is NO `stop_reason`** in the hook payload. Drop the assumption.
  `stop_reason` IS available inside the transcript's last assistant line
  (`message.stop_reason: "end_turn"`) if needed.
- `transcript_path` is provided; the wrapper records it on the first hook for
  `GET /v1/transcript`.

Fixture: `cmd/cc-stop-hook/testdata/hook_payload.json` (path/cwd sanitized to
`/workspace`).

## 5. Transcript JSONL (real)

Append-only JSONL. Assistant lines:
`{"type":...,"message":{"role":"assistant","content":[{"type":"text","text":"PING"}],"stop_reason":"end_turn","usage":{...}}}`
The `lastAssistantText` parser (concatenate `content[].text` of the last
assistant line; grab `message.usage`) is correct. `usage` is rich
(input_tokens, output_tokens, cache_*). Fixtures:
`cmd/cc-stop-hook/testdata/transcript.jsonl`,
`internal/session/testdata/transcript_assistant_line.jsonl`.

## 6. Readiness

After seeding, the prompt is ready within a few seconds. v0.1.0 uses a bounded
boot wait. A tighter signal (watch PTY output for the input box) is a possible
enhancement but not required.
