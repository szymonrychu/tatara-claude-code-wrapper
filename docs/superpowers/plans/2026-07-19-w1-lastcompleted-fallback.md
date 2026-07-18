# W1: currentTurnID lastCompleted fallback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop the wrapper from silently dropping every end-of-turn `report_internal_issue` call by making `Manager.currentTurnID()` fall back to the just-completed turn id for the brief window after a turn clears but before the transcript tailer has caught up.

**Architecture:** Add a `lastCompleted string` field to `session.Manager`, stamped by `clearCurrentLocked` at the single site that already covers every terminal path (Complete, failTurn, failTimeout, failSubmitWrite, completeFromTranscript). `currentTurnID()` (the tailer's sole turn-id source) returns `current` when non-empty, else `lastCompleted`. This closes the race without touching `#107`'s per-turn keying or `#105`'s `CaughtUpTo` catch-up wait - both are preserved as-is.

**Tech Stack:** Go 1.25, `testify/require`, existing `transcript.Tailer` / `session.Manager` test harnesses.

## Global Constraints

- Repo: `tatara-claude-code-wrapper` @ `578182b` (main). Implementation runs in the existing worktree `/Users/szymonri/Documents/tatara-new/.worktrees/fix/381-lifecycle-races/tatara-claude-code-wrapper` on branch `fix/381-lifecycle-races` (already created, HEAD `578182b`, no changes yet).
- Toolchain: mise-pinned only - `mise exec -- go ...` / `mise run test` / `mise run lint`, never a bare `go`. `mise install` once if tools are not yet installed in the worktree.
- Build/verify commands: `mise run test` (= `go test ./... -race -count=1`), `mise run lint` (= `golangci-lint run ./...`), `make build` for the compile check. `pre-commit run --all-files` before commit.
- Go rules: `gofmt` clean, wrap errors with `%w` (n/a here, no new error paths), table-driven tests with `t.Run` where the existing file already uses that pattern (it does not for `session_test.go`'s style - follow the surrounding file's existing per-function test style instead; do not introduce a new pattern into a file that does not use it).
- No docstrings/comments on unchanged code. Comments only on the new field/logic, explaining the *why* (the race), not the *what*.
- `change_significance: patch` on the eventual PR's `change_summary` (bugfix, no API/behavior contract change for callers).
- Known residual explicitly OUT OF SCOPE: `cmd/wrapper/app.go:239` (outcome-reprompt early-return drops `rec.InternalIssues` for that turn) - do not touch it in this plan.
- MEMORY.md entry required (dated, one paragraph, non-obvious decision: why a fallback field instead of e.g. draining before clearing).

---

## Verified file:line map (re-checked against `578182b`, all match the design doc within 1-5 lines)

- `internal/session/session.go:147` - `current string` field (struct block: `current` at 147, `currentStarted` 148, `currentSessionID` 149).
- `internal/session/session.go:216-220` - `currentTurnID()`.
- `internal/session/session.go:1101-1123` - `Complete()`; `mgr.clearCurrentLocked(Ready)` at line 1110, called **before** `mgr.fireDone(rec)` at line 1121 (this ordering is what the fallback compensates for; it is not being reordered).
- `internal/session/session.go:1185-1190` - `clearCurrentLocked`.
- `internal/session/session.go:1279-1298` - `DrainInternalIssues` (unchanged by this plan; already has the `#105` `CaughtUpTo` wait).
- `internal/session/session.go:197-214` - `StartTailer` (wires `mgr.currentTurnID` into `transcript.NewTailer` at line 202; unchanged).
- `internal/session/session.go:229-237` - `onTailerActivity` (unchanged; only consumer of `mgr.current` besides `currentTurnID`/`Submit`/`clearCurrentLocked` call sites - confirmed it reads `mgr.current` directly, not `currentTurnID()`, so it is unaffected by this change).
- `internal/session/session.go:331-335` - `SetTailerForTest`.
- `internal/transcript/tailer.go:271-279` - `accumulateInternalIssue` (resets accumulator on turn-id change; unchanged, this is what the fallback prevents from firing spuriously on the trailing report).
- `internal/transcript/tailer.go:285-295` - `Tailer.DrainInternalIssues` (unchanged).
- `internal/transcript/tailer.go:501-517` - `processLine`, captures `turnID := t.turnID()` once per line (line 512) and calls `t.fireActivity(turnID)` (line 516) before any tool dispatch.
- `internal/transcript/tailer_test.go:1127-1131` - `makeInternalIssueLine(inputJSON string) string` helper (package `transcript`, **not** importable from `session_test`; the new session-level tests build their own inline JSON line).
- `cmd/wrapper/app.go:250` - `rec.InternalIssues = a.sess.DrainInternalIssues(rec.ID)` (unchanged; `rec.ID` is always the real non-empty turn id here, confirming the bug is entirely on the tailer/session side, not a bad id being passed).
- `cmd/wrapper/app.go:239` - the out-of-scope outcome-reprompt early return (see Global Constraints).
- `internal/session/currentTurnID` sole caller confirmed via repo-wide grep: only `session.go:202`.

Drift from the binding design doc: none material. The design said "tailer_test.go:1128 internalIssueLine helper" without a package qualifier - it lives in `internal/transcript/tailer_test.go` as `makeInternalIssueLine`, in package `transcript`. Since `internal/session/session_test.go` is `package session_test` (external), it cannot call that helper directly; Task 2 below inlines an equivalent JSON line literal instead (same shape, same `mcp__tatara__report_internal_issue` tool name).

## File Structure

- Modify `internal/session/session.go`: add `lastCompleted` field (~line 148), update `clearCurrentLocked` (~line 1185), update `currentTurnID` (~line 216).
- Create `internal/session/lastcompleted_test.go` (package `session`, white-box - mirrors the existing white-box precedent `internal/session/ring_test.go`): unit test for the fallback logic directly against `mgr.current`/`mgr.lastCompleted`/`clearCurrentLocked`/`currentTurnID`, none of which are exported.
- Modify `internal/session/session_test.go` (package `session_test`, black-box): add the RED-then-GREEN integration test and the mid-turn/no-leak regression test, using the existing `newMgr` helper and the real `StartTailer`/`Complete`/`DrainInternalIssues` production wiring (no new test-only exported setters needed).
- Modify `MEMORY.md` (repo root): one dated entry.

## Task 1: RED - white-box unit test for the fallback

**Files:**
- Create: `internal/session/lastcompleted_test.go`

**Interfaces:**
- Consumes: `session.New`, `session.Config`, `session.Ready` (all already exported), and the unexported `Manager.mu`, `Manager.current`, `Manager.clearCurrentLocked`, `Manager.currentTurnID` (accessible because this file is `package session`, matching the existing white-box precedent in `ring_test.go`).
- Produces: nothing consumed by later tasks - this is a leaf unit test.

- [ ] **Step 1: Write the failing test**

```go
package session

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

// TestCurrentTurnID_FallsBackToLastCompletedAfterClear reproduces the wrapper
// side of tatara-operator#381 (W1): the transcript tailer's turnID() source
// is currentTurnID(), which currently returns mgr.current directly. Every
// clearCurrentLocked call (Complete/failTurn/failTimeout/failSubmitWrite/
// completeFromTranscript) sets mgr.current = "" BEFORE the tailer's Follow
// goroutine has necessarily processed the turn's trailing transcript lines
// (poll-interval race, see DrainInternalIssues' CaughtUpTo wait). A
// report_internal_issue call that is the turn's last transcript line then
// gets stamped turnID="" instead of the real turn id, and
// accumulateInternalIssue resets the accumulator under iiTurnID="" -
// DrainInternalIssues(realTurnID) never matches it. Fix: currentTurnID falls
// back to the last-completed turn id while no new turn is in flight.
func TestCurrentTurnID_FallsBackToLastCompletedAfterClear(t *testing.T) {
	mgr := New(Config{}, turn.NewStore(), metrics.New(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now, func() string { return "" })

	mgr.mu.Lock()
	mgr.current = "turn-1"
	mgr.clearCurrentLocked(Ready)
	mgr.mu.Unlock()

	if got := mgr.currentTurnID(); got != "turn-1" {
		t.Errorf("currentTurnID() after clear = %q, want %q (lastCompleted fallback)", got, "turn-1")
	}
}

// TestCurrentTurnID_NewCurrentWinsOverLastCompleted verifies the fallback is
// bypassed the instant a new turn is reserved: a stale lastCompleted from
// turn N must never attribute a turn-N+1 transcript line.
func TestCurrentTurnID_NewCurrentWinsOverLastCompleted(t *testing.T) {
	mgr := New(Config{}, turn.NewStore(), metrics.New(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now, func() string { return "" })

	mgr.mu.Lock()
	mgr.current = "turn-1"
	mgr.clearCurrentLocked(Ready)
	mgr.current = "turn-2" // simulates the next Submit reserving the slot
	mgr.mu.Unlock()

	if got := mgr.currentTurnID(); got != "turn-2" {
		t.Errorf("currentTurnID() with new current = %q, want %q", got, "turn-2")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/szymonri/Documents/tatara-new/.worktrees/fix/381-lifecycle-races/tatara-claude-code-wrapper && mise exec -- go test ./internal/session/... -run TestCurrentTurnID -v`
Expected: `TestCurrentTurnID_FallsBackToLastCompletedAfterClear` FAILS with `currentTurnID() after clear = "", want "turn-1"`. `TestCurrentTurnID_NewCurrentWinsOverLastCompleted` PASSES already (current wins trivially pre-fix too) - that is fine, it is a forward-looking regression guard, not required to be RED.

- [ ] **Step 3: Add the `lastCompleted` field**

In `internal/session/session.go`, in the `Manager` struct (around line 147-149):

```go
	current          string    // in-flight turn id, "" when idle
	// lastCompleted is the turn id most recently cleared by
	// clearCurrentLocked. currentTurnID() falls back to it while current is
	// "" so a transcript line the tailer processes in the brief window
	// between a turn clearing and the tailer catching up (poll-interval
	// race, tatara-operator#381 W1) is still attributed to the turn that
	// produced it, instead of being silently stamped with turnID="" and
	// dropped by accumulateInternalIssue's turn-change reset. Overwritten by
	// the NEXT clearCurrentLocked call; never read once a new turn sets
	// current again (currentTurnID prefers current unconditionally).
	lastCompleted    string
	currentStarted   time.Time // original Submit time; basis for TurnDuration metric
```

- [ ] **Step 4: Update `clearCurrentLocked`**

Replace (session.go:1185-1190):

```go
func (mgr *Manager) clearCurrentLocked(next State) {
	mgr.current = ""
	mgr.turnsCompleted++ // all terminal turns (success + failed + timed-out)
	mgr.state = next
	mgr.m.TurnInFlight.Set(0)
}
```

with:

```go
func (mgr *Manager) clearCurrentLocked(next State) {
	mgr.lastCompleted = mgr.current
	mgr.current = ""
	mgr.turnsCompleted++ // all terminal turns (success + failed + timed-out)
	mgr.state = next
	mgr.m.TurnInFlight.Set(0)
}
```

- [ ] **Step 5: Update `currentTurnID`**

Replace (session.go:216-220):

```go
func (mgr *Manager) currentTurnID() string {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return mgr.current
}
```

with:

```go
func (mgr *Manager) currentTurnID() string {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if mgr.current != "" {
		return mgr.current
	}
	return mgr.lastCompleted
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `cd /Users/szymonri/Documents/tatara-new/.worktrees/fix/381-lifecycle-races/tatara-claude-code-wrapper && mise exec -- go test ./internal/session/... -run TestCurrentTurnID -v`
Expected: both tests PASS.

- [ ] **Step 7: Commit**

```bash
cd /Users/szymonri/Documents/tatara-new/.worktrees/fix/381-lifecycle-races/tatara-claude-code-wrapper
git add internal/session/session.go internal/session/lastcompleted_test.go
git commit -m "fix: fall back currentTurnID to last-completed turn after clear"
```

## Task 2: RED-then-GREEN - integration test reproducing the real drop end-to-end

**Files:**
- Modify: `internal/session/session_test.go` (append new tests; do not reorder existing ones)

**Interfaces:**
- Consumes: `newMgr(t, fp) (*session.Manager, *turn.Store)` (existing helper, session_test.go:48-58), `session.Manager.StartTailer(ctx)`, `session.Manager.Submit(text, cb, handoff) (string, error)`, `session.Manager.Complete(session.HookResult) error`, `session.Manager.DrainInternalIssues(turnID string) []turn.InternalIssueReport`, `turn.InternalIssueReport{Category, Severity, Description, OffendingTool, ResourceID}` (turn package, unchanged).
- Produces: nothing consumed by later tasks.

This test does not need `SetTailerForTest` - it drives the REAL production wiring (`StartTailer` + `Complete`'s first-hook tailer-launch at session.go:1064-1076), which is what actually contains the race. The determinism trick: `Complete()` runs `mgr.store.Complete` -> `mgr.clearCurrentLocked` synchronously and returns BEFORE the test calls `DrainInternalIssues`, so `mgr.current` is guaranteed already `""` by the time the drain runs - the race is not "will it happen", it is "does the fallback exist to survive it". This makes the test reliable under `-race` with no sleeps required beyond `DrainInternalIssues`'s own internal `CaughtUpTo` wait (bounded by `internalIssueCatchUpTimeout` = 2s, session.go:1266).

- [ ] **Step 1: Write the failing test**

Append to `internal/session/session_test.go`:

```go
// TestDrainInternalIssues_TrailingReportSurvivesTurnClear reproduces
// tatara-operator#381 (W1): a report_internal_issue call that is the LAST
// line of a turn's transcript is written to disk before/around the Stop
// hook POST, but the tailer's Follow goroutine (200ms poll) has not
// necessarily read it by the time Complete() runs. Complete() clears
// mgr.current synchronously before returning; DrainInternalIssues is then
// called (mirroring app.go's finalizeTurn) with the real, non-empty turn id
// while the tailer is still catching up. Pre-fix, the tailer processes the
// trailing line with turnID()="" (mgr.current already cleared) and
// DrainInternalIssues(realID) never matches - the report is silently
// dropped. Post-fix, currentTurnID's lastCompleted fallback keeps the
// trailing line attributed to the turn that produced it.
func TestDrainInternalIssues_TrailingReportSurvivesTurnClear(t *testing.T) {
	fp := &fakePTY{}
	m, _ := newMgr(t, fp)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.StartTailer(ctx)

	id, err := m.Submit("hi", "https://cb/x", false)
	require.NoError(t, err)
	require.Equal(t, "turn-1", id)

	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	// The trailing line is ALREADY on disk at the moment Complete() is
	// called, exactly as it would be in production: claude writes the
	// report_internal_issue tool_use, then the Stop hook fires. The tailer's
	// Follow goroutine only starts inside Complete() (first-hook launch,
	// session.go:1064-1076) so it has read nothing yet.
	line := `{"type":"assistant","uuid":"u1","sessionId":"s1","timestamp":"2026-07-19T00:00:00.000Z",` +
		`"message":{"role":"assistant","content":[{"type":"tool_use","id":"t1",` +
		`"name":"mcp__tatara__report_internal_issue","input":` +
		`{"category":"workspace_broken","severity":"error","description":"stuck mid-review"}}],` +
		`"stop_reason":"tool_use"}}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(line), 0o644))

	require.NoError(t, m.Complete(session.HookResult{
		FinalText: "done", StopReason: "end_turn", TranscriptPath: path,
	}))

	got := m.DrainInternalIssues(id)
	require.Len(t, got, 1, "trailing report_internal_issue must survive the turn clearing before the tailer caught up")
	require.Equal(t, turn.InternalIssueReport{
		Category: "workspace_broken", Severity: "error", Description: "stuck mid-review",
	}, got[0])
}

// TestDrainInternalIssues_MidTurnAttributionUnaffectedByFallback is a
// regression guard: a report processed WHILE its turn is still in flight
// (the tailer has already caught up before Complete runs, the pre-existing
// #105/#107 behavior) must keep working unchanged, and a second turn's own
// report must not be shadowed by the first turn's already-drained one.
func TestDrainInternalIssues_MidTurnAttributionUnaffectedByFallback(t *testing.T) {
	fp := &fakePTY{}
	m, _ := newMgr(t, fp)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.StartTailer(ctx)

	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(path, nil, 0o644))

	id1, err := m.Submit("hi", "https://cb/x", false)
	require.NoError(t, err)
	require.Equal(t, "turn-1", id1)
	require.NoError(t, m.Complete(session.HookResult{
		FinalText: "done", StopReason: "end_turn", TranscriptPath: path,
	})) // starts the real tailer Follow goroutine on the (empty) file

	id2, err := m.Submit("hi again", "https://cb/x", false)
	require.NoError(t, err)
	require.Equal(t, "turn-2", id2)

	line := `{"type":"assistant","uuid":"u2","sessionId":"s1","timestamp":"2026-07-19T00:01:00.000Z",` +
		`"message":{"role":"assistant","content":[{"type":"tool_use","id":"t2",` +
		`"name":"mcp__tatara__report_internal_issue","input":` +
		`{"category":"auth","severity":"warn","description":"turn two issue"}}],` +
		`"stop_reason":"tool_use"}}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(line)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	// Give the already-running Follow goroutine a full poll cycle to read
	// and attribute this line to turn-2 WHILE turn-2 is still in flight -
	// the genuine mid-turn (non-fallback) path.
	time.Sleep(300 * time.Millisecond)

	require.NoError(t, m.Complete(session.HookResult{
		FinalText: "done2", StopReason: "end_turn", TranscriptPath: path,
	}))

	got := m.DrainInternalIssues(id2)
	require.Len(t, got, 1)
	require.Equal(t, "turn two issue", got[0].Description)

	// turn-1 reported nothing and must still drain empty (no leakage from
	// the fallback or from turn-2's report).
	require.Nil(t, m.DrainInternalIssues(id1))
}
```

Add `"context"` and `"time"` to the existing import block if not already present (both already are - see session_test.go:5,15).

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/szymonri/Documents/tatara-new/.worktrees/fix/381-lifecycle-races/tatara-claude-code-wrapper && mise exec -- go test ./internal/session/... -run TestDrainInternalIssues_TrailingReportSurvivesTurnClear -v -race`
Expected (BEFORE Task 1's fix is applied): FAIL - `got` has length 0, not 1. Since Task 1 already landed the fix in this same worktree by the time this step runs, this step must be executed by TEMPORARILY reverting the Task 1 fix (`git stash` the `session.go` hunk, or check out the pre-fix version of `currentTurnID`/`clearCurrentLocked`) to confirm RED, then restoring it. Record the confirmed-RED output before moving on; do not skip this verification.

`TestDrainInternalIssues_MidTurnAttributionUnaffectedByFallback` PASSES even pre-fix (it does not exercise the race) - confirm this too, so it is understood as a regression guard, not part of the RED reproduction.

- [ ] **Step 3: Restore the Task 1 fix, run tests to verify they pass**

Run: `cd /Users/szymonri/Documents/tatara-new/.worktrees/fix/381-lifecycle-races/tatara-claude-code-wrapper && mise exec -- go test ./internal/session/... -run TestDrainInternalIssues -v -race`
Expected: all `TestDrainInternalIssues_*` tests PASS, including both new ones.

- [ ] **Step 4: Commit**

```bash
cd /Users/szymonri/Documents/tatara-new/.worktrees/fix/381-lifecycle-races/tatara-claude-code-wrapper
git add internal/session/session_test.go
git commit -m "test: reproduce W1 trailing internal-issue report drop"
```

## Task 3: Full verification + MEMORY.md

**Files:**
- Modify: `MEMORY.md` (repo root)

**Interfaces:**
- Consumes: nothing new.
- Produces: nothing consumed by later tasks - final task.

- [ ] **Step 1: Full test suite under race**

Run: `cd /Users/szymonri/Documents/tatara-new/.worktrees/fix/381-lifecycle-races/tatara-claude-code-wrapper && mise run test`
Expected: PASS, no new failures anywhere in the module (confirms the fallback does not regress `TestManager_DrainInternalIssues_NoTailerReturnsNil`, `TestManager_DrainInternalIssues_TimeoutIncrementsCounter`, `TestManager_DrainInternalIssues_CaughtUpDoesNotIncrementCounter`, `TestComplete_*`, `shouldresume_test.go`, `recover_test.go`, `restart_hook_test.go`, and everything in `cmd/wrapper`).

- [ ] **Step 2: Lint**

Run: `cd /Users/szymonri/Documents/tatara-new/.worktrees/fix/381-lifecycle-races/tatara-claude-code-wrapper && mise run lint`
Expected: clean (exit 0, or exit 5 with no issues per the Makefile's `|| [ $$? -eq 5 ]` convention).

- [ ] **Step 3: Build**

Run: `cd /Users/szymonri/Documents/tatara-new/.worktrees/fix/381-lifecycle-races/tatara-claude-code-wrapper && make build`
Expected: succeeds, no compile errors.

- [ ] **Step 4: Add MEMORY.md entry**

Read `MEMORY.md` first, then append a new dated entry after the last existing entry (matches this repo's existing MEMORY.md style: dated paragraph, decision + rationale):

```markdown
2026-07-19: **W1 (tatara-operator#381) - currentTurnID lastCompleted fallback.** `Manager.currentTurnID()` (the transcript tailer's sole turn-id source) returned `mgr.current` directly, which `clearCurrentLocked` zeroes BEFORE the tailer catches up (poll-interval race preserved from #105/#107). A `report_internal_issue` call that is a turn's last transcript line got stamped `turnID=""` by the tailer and silently dropped by `accumulateInternalIssue`'s turn-change reset - every end-of-turn report, 100% loss, confirmed operator-side by zero `agent_internal_issue` log lines across 7 days of live turn-complete callbacks despite agents actively filing reports. Considered draining before clearing instead (reorder `Complete`'s `clearCurrentLocked`/`fireDone`), rejected: `DrainInternalIssues` already runs asynchronously off `fireDone` in `app.go`'s `finalizeTurn` goroutine, well after `Complete` returns and the lock releases - there is no synchronous point left to drain at without blocking the cc-stop-hook HTTP response, which #105's whole `CaughtUpTo` mechanism was built to avoid. Chose instead: a `lastCompleted string` field, stamped by `clearCurrentLocked` (the single site covering every terminal path - Complete/failTurn/failTimeout/failSubmitWrite/completeFromTranscript), and `currentTurnID` prefers `current` but falls back to `lastCompleted` when idle. Overwritten on the next clear; a new Submit's `current` always wins immediately, so no stale-turn leakage into the next turn (regression-tested). Known residual, deliberately out of scope: `cmd/wrapper/app.go:239`'s outcome-reprompt early return still drops `rec.InternalIssues` for that turn - separate bug, no shared root cause with W1.
```

- [ ] **Step 5: Commit**

```bash
cd /Users/szymonri/Documents/tatara-new/.worktrees/fix/381-lifecycle-races/tatara-claude-code-wrapper
git add MEMORY.md
git commit -m "docs: record W1 lastCompleted fallback decision in MEMORY.md"
```

## Task 4: PR readiness (handoff to github-pullrequests-update / watch skills - not scripted here)

Not a code task - noted for the executing session:
- Push branch `fix/381-lifecycle-races`, open PR against `tatara-claude-code-wrapper` main.
- PR body: reference `tatara-operator#381`, summarize the drop + fix, note `change_significance: patch` (or apply a `semver:patch` label).
- Cross-link to the companion `tatara-operator` PR (fixes A + B from the same investigation, delivered separately per the binding design doc's "Delivery" section) once both exist.
- Watch CI via `github-pullrequests-watch`; this repo's pipeline runs `make build test`.

---

## Self-Review

**Spec coverage against binding design (stream2-design.md W1 section):**
- `lastCompleted string` field next to `current` (~:147) - Task 1 Step 3. Covered.
- `clearCurrentLocked`: `mgr.lastCompleted = mgr.current` before clearing `mgr.current` - Task 1 Step 4. Covered.
- `currentTurnID`: return `current` if non-empty, else `lastCompleted` - Task 1 Step 5. Covered.
- Sole caller verified (tailer turnID closure, session.go:202) - confirmed via repo-wide grep in the verified file:line map section above; no other effect. Covered.
- Preserves #107 per-turn keying + #105 CaughtUpTo - no changes made to `accumulateInternalIssue`, `DrainInternalIssues` (either the Manager or Tailer version), or `CaughtUpTo`. Covered by omission (explicitly not touched).
- Unit test (clearCurrentLocked keeps currentTurnID returning old id until next Submit) - Task 1, both tests. Covered.
- Integration test via SetTailerForTest + tailer_test harness (RED pre-fix) - reinterpreted: `SetTailerForTest` is unnecessary and less faithful than driving the real `StartTailer`/`Complete` production wiring, which is what actually contains the race; `tailer_test.go`'s `makeInternalIssueLine` helper is unexported in package `transcript` and unreachable from `session_test`, so Task 2 inlines an equivalent JSON line. Same intent, adjusted mechanism - noted explicitly in Task 2's preamble and the drift note above. Covered.
- Mid-turn + end-of-turn both drained - Task 2's two tests (trailing-report / mid-turn-unaffected). Covered.
- Known residual OUT OF SCOPE: app.go:239 - called out in Global Constraints, MEMORY.md entry, and Task 2 is scoped away from it. Covered.
- change_significance: patch - Task 4 note + Global Constraints. Covered.
- MEMORY.md dated entry (non-obvious decision: lastCompleted fallback choice) - Task 3 Step 4. Covered.
- Toolchain: mise exec/run only; `make build test` - Task 3 Steps 1-3, and every Run line throughout. Covered.

**Placeholder scan:** no TBD/TODO, no "add appropriate handling", no "similar to Task N" without repeated code, no bare-description code steps. All code blocks are complete and copy-pasteable.

**Type consistency:** `turn.InternalIssueReport{Category, Severity, Description, OffendingTool, ResourceID}` used identically in Task 2's assertions as defined in `turn` package (confirmed against the existing `TestTailer_DrainInternalIssues_AccumulatesAndDrains` usage in `tailer_test.go:1570-1573`, same field names/order). `session.HookResult{FinalText, StopReason, TranscriptPath}` matches the struct at session.go:73-86. `newMgr(t, fp) (*session.Manager, *turn.Store)` matches its existing definition (session_test.go:48-58) exactly, including that it pre-seeds `ids <- "turn-1"; ids <- "turn-2"` so `Submit` calls in Task 2 deterministically get `"turn-1"` then `"turn-2"` without a third `newMgr` call - both new tests in Task 2 rely on exactly two Submits per manager instance, consistent with that seeding.
