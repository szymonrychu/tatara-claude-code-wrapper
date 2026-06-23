# Spike findings - cross-pod conversation resume (issue #114)

Captured 2026-06-23 against `claude` **v2.1.183** in the agent container,
empirically (two isolated `$HOME`s, fresh transcripts, real turns). These pin
the restore mechanism that P1 (S3 conversation persistence) builds on. Where
they differ from the issue-114 triage sketch, these win.

## TL;DR (the chosen mechanism)

A new pod resumes a prior conversation by placing exactly ONE file on disk and
passing one flag:

1. Write the conversation transcript to
   `~/.claude/projects/<mangled-cwd>/<sessionId>.jsonl`.
2. Launch `claude --resume <sessionId>`.

Nothing else is required - not `sessions/`, not `session-env/`, not the
`~/.claude.json` `projects` entry (trust seeding is already handled by
`bootstrap/claudejson.go`). Confirmed by a secret-recall test across two fresh
`$HOME`s where the second pod had ONLY the copied `.jsonl`.

## 1. The "cwd-hash" is not a hash - it is path mangling

Claude stores transcripts under `~/.claude/projects/<dir>/` where `<dir>` is the
ABSOLUTE cwd with every `/` replaced by `-`. Verified:

| cwd | project dir |
|---|---|
| `/workspace` | `~/.claude/projects/-workspace/` |
| `/tmp/spike-ws` | `~/.claude/projects/-tmp-spike-ws/` |

There is NO content hashing and no version in the dir name, so it is fully
deterministic and IDENTICAL across pods as long as cwd is identical. tatara
always runs claude with `cmd.Dir = cfg.Workspace` (default `/workspace`, see
`internal/session/pty.go`), so the project dir is always `-workspace`. This is
better than the triage's "cwd-hash must hash identically" worry: there is no
hash to collide or drift.

Implication: the transcript line also records `cwd` per message
(`/workspace`). `--resume` looks the conversation up by scanning the CURRENT
cwd's project dir, so the downloaded `.jsonl` must be placed under the dir
matching the NEW pod's cwd. Since cwd is always `/workspace`, this always lines
up.

## 2. Cross-pod resume: CONFIRMED

Test: pod 1 (`HOME=/tmp/h1`) created a conversation with `--session-id <SID>`
containing a secret pass-phrase. A fresh pod 2 (`HOME=/tmp/h2`, nothing else)
received ONLY `<SID>.jsonl` copied into its project dir, then ran
`claude --resume <SID> -p "what was the pass-phrase?"`. It answered correctly.
So a transcript downloaded from S3 is sufficient to restore full context.

## 3. Resume writeback: sessionId is STABLE

Plain `claude --resume <SID>` APPENDS to the same `<SID>.jsonl` (file grew in
place; no new file). So across N resume hops the sessionId and the object key
stay constant - upload the same `<SID>.jsonl` after every turn. No key churn.

## 4. Forking (issue #114 decision 3): `--fork-session`

`claude --resume <SID> --fork-session` creates a NEW sessionId, writes a NEW
`<newSID>.jsonl`, and leaves the parent `<SID>.jsonl` UNTOUCHED, while still
inheriting the parent's context (secret recall still worked). This IS the fork
primitive for "one brainstorm conversation, forked per proposed issue": the
child pod downloads the parent transcript, launches with `--resume <parentSID>
--fork-session`, and the new child sessionId (discovered via the Stop hook, see
below) is uploaded under the child issue's own key. This is cleaner than an S3
copy-object because claude assigns the fork its own id and the divergence is
native; subtask 8 should prefer it over copy-object.

## 5. sessionId discovery

On the FIRST run of a conversation claude generates a random sessionId. The
wrapper already learns it from the Stop hook payload (`session_id` +
`transcript_path`, see `docs/spike-findings.md` section 4 and
`cmd/cc-stop-hook`). So no extra discovery is needed: capture `session_id` at
turn end, report it to the operator (subtask 6), and reuse it as the S3 object
key suffix. `--session-id <uuid>` can optionally FORCE a deterministic id on
first run (e.g. derived from the issue) if a fixed key scheme is preferred, but
hook-discovery already suffices.

## 6. Transcript schema + version coupling

Per-line JSON `type` in {user, assistant, attachment, system, ai-title,
last-prompt, mode, permission-mode, file-history-snapshot, queue-operation}.
user/assistant/attachment lines carry a per-line `version` field equal to the
claude version (`2.1.183`). The wrapper's `internal/transcript/result.go` reads
only `type`, `message.content[].text`, `message.stop_reason`, and
`message.usage.*` - it does NOT validate `version`, `cwd`, or `sessionId`
against the environment, so a transplanted transcript parses fine.

The one real coupling: resume is exercised within a single wrapper image, i.e.
a single claude version. A conversation that spans a wrapper-image upgrade would
have a newer claude resuming an older transcript. claude is generally
backward-compatible for resume, but this is the failure mode to guard. Mitigation
is already in the plan: the 25%-context compaction/handover fallback (subtask 7)
means a failed full-resume degrades to the text handover rather than losing the
thread. P1 should treat a resume launch failure as "fall back to fresh + handover",
not a hard error.

## 7. What P1 must copy across pods

ONLY `~/.claude/projects/-workspace/<sessionId>.jsonl`. Do NOT bother with:
- `~/.claude/sessions/<pid>.json` - pid-keyed LIVE-process registry, irrelevant
  to a new pod.
- `~/.claude/session-env/<sessionId>` - empty in practice.
- `~/.claude.json` `projects["/workspace"]` - only onboarding/trust flags, already
  seeded by bootstrap.
- the `<sessionId>/subagents/*.jsonl` sub-transcripts - subagent history; NOT
  needed to resume the main conversation (out of scope for P1; revisit only if
  subagent continuity is ever required).
