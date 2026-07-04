// Package session supervises one interactive claude process over a PTY and
// turns API submissions into typed turns, correlating Stop-hook callbacks.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/convstore"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/transcript"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

// goroutineJoinTimeout is the maximum time Shutdown waits for lifecycle
// goroutines (readPTY, watch, tailer Follow) to exit after cancellation.
const goroutineJoinTimeout = 5 * time.Second

// DefaultSubmitSeq wraps a message in bracketed paste, then submits with CR.
// Confirmed/overridden by the Task-1 spike.
var DefaultSubmitSeq = SubmitSequence{PasteStart: "\x1b[200~", PasteEnd: "\x1b[201~", Submit: "\r"}

type SubmitSequence struct{ PasteStart, PasteEnd, Submit string }

var ErrBusy = errors.New("session busy")

// ErrNotBusy is returned by Interject when there is no in-flight turn to
// inject into: an interjection only makes sense while a turn is running.
var ErrNotBusy = errors.New("no in-flight turn to interject")

type Config struct {
	ClaudePath string
	Workspace  string
	// HomeDir is claude's $HOME on this pod. Used only to locate
	// ~/.claude/projects/<dir>/*.jsonl (convstore.TranscriptDir) for the
	// shouldResume on-disk transcript check; "" disables that check (no false
	// resume from a directory we cannot compute).
	HomeDir     string
	Env         []string
	Model       string
	Effort      string
	Repo        string // primary repository URL the pod is bound to ("" if none)
	TurnTimeout time.Duration
	BootTimeout time.Duration
	SubmitDelay time.Duration // pause between the paste and the submit keystroke
	SubmitSeq   SubmitSequence
	MaxRestarts int // crash-relaunch budget per session; default 3

	// Kind, RepoName, Project are the pod's metric-identity labels (component 6),
	// set once by the operator env and stamped onto every per-turn token/cost
	// series so spend attributes to a Task kind, repo, and project.
	Kind     string
	RepoName string
	Project  string
}

// HookResult is the payload cc-stop-hook POSTs to the internal endpoint.
type HookResult struct {
	SessionID      string          `json:"sessionId"`
	FinalText      string          `json:"finalText"`
	ResultJSON     json.RawMessage `json:"resultJson,omitempty"`
	Usage          json.RawMessage `json:"usage,omitempty"`
	StopReason     string          `json:"stopReason"`
	TranscriptPath string          `json:"transcriptPath,omitempty"`
	// TurnTokens is the token usage summed across every assistant message of the
	// just-completed turn, grouped by model. The single last-message Usage above
	// undercounts agentic turns (it keeps only the final step), so the metric is
	// driven by this summed view instead. Computed by the stop hook, which already
	// reads the whole transcript.
	TurnTokens []TurnTokens `json:"turnTokens,omitempty"`
}

// TurnTokens is the per-model token total for one turn. Aliased to the
// transcript package, which owns the JSONL parsing that produces it (both the
// stop hook and the crash-recovery completion path build it from the transcript).
type TurnTokens = transcript.TurnTokens

type State string

const (
	Booting State = "booting"
	Ready   State = "ready"
	Busy    State = "busy"
	Dead    State = "dead"
)

type Snapshot struct {
	State          State     `json:"state"`
	TurnsCompleted int       `json:"turnsCompleted"` // successful turns only (excludes failed/timed-out)
	TurnsFinished  int       `json:"turnsFinished"`  // all terminal turns (success + failed + timed-out)
	Model          string    `json:"model"`
	Repo           string    `json:"repo"`
	LastActivityAt time.Time `json:"lastActivityAt"` // last agent_stream event of the in-flight turn; zero when idle
}

type Manager struct {
	cfg   Config
	store *turn.Store
	m     *metrics.Metrics
	log   *slog.Logger
	now   func() time.Time
	newID func() string

	OnTurnDone func(*turn.Record)

	// OnRestart, when set, is invoked once after a crash-relaunch that resumed an
	// existing conversation (--continue). It is NOT called for a fresh first-boot
	// relaunch (nothing to resume). Invoked synchronously from the watch
	// goroutine, so the callback must not block (the app wiring fires it in a
	// goroutine).
	OnRestart func()

	spawn func(cfg Config, resume bool) (claudeProcess, error)

	mu               sync.Mutex
	w                ptyWriter
	proc             claudeProcess
	ring             *ringBuffer
	stopping         bool
	state            State
	current          string    // in-flight turn id, "" when idle
	currentStarted   time.Time // original Submit time; basis for TurnDuration metric
	currentSessionID string    // claude's sessionId for the current turn (from hook); used for correlation
	timer            *time.Timer
	turnsCompleted   int // all terminal turns (success + failed + timed-out)
	turnsSucceeded   int // successful turns only (Complete path)
	transcriptPath   string
	restarts         int // consecutive crash-relaunches; reset on Complete

	// wg tracks lifecycle goroutines (readPTY, watch, tailer Follow) so
	// Shutdown can wait for them to drain after cancellation.
	wg sync.WaitGroup

	// tailer goroutine
	tailer        *transcript.Tailer
	tailerParent  context.Context // session-lifetime context; source for new per-path child contexts
	tailerCtx     context.Context
	tailerCancel  context.CancelFunc
	tailerStarted bool
}

func New(cfg Config, store *turn.Store, m *metrics.Metrics, log *slog.Logger, now func() time.Time, newID func() string) *Manager {
	if cfg.BootTimeout <= 0 {
		cfg.BootTimeout = 60 * time.Second
	}
	if cfg.SubmitDelay <= 0 {
		cfg.SubmitDelay = 400 * time.Millisecond
	}
	if cfg.MaxRestarts <= 0 {
		cfg.MaxRestarts = 3
	}
	mgr := &Manager{cfg: cfg, store: store, m: m, log: log, now: now, newID: newID, state: Booting, ring: newRing()}
	mgr.spawn = func(cfg Config, resume bool) (claudeProcess, error) {
		p, err := spawnClaude(cfg, resume)
		if err != nil {
			return nil, err
		}
		return p, nil
	}
	return mgr
}

// StartTailer sets up the transcript tailer goroutine. It must be called before
// the first Complete() that supplies a transcript path. ctx governs tailer
// lifetime; cancel it (or let it expire) to stop the tailer. Calling
// StartTailer is a no-op when CCW_LOG_TRANSCRIPT=false.
func (mgr *Manager) StartTailer(ctx context.Context) {
	if !logTranscriptEnabled() {
		return
	}
	redactor := transcript.NewRedactor(secretsFromEnv())
	tailer := transcript.NewTailer(mgr.log, redactor, mgr.currentTurnID)
	tailer.WithCounter(mgr.m.StreamEventsTotal)
	tailer.WithInternalIssueCounter(mgr.m.InternalIssueTotal)
	tailer.WithToolCallsCounter(mgr.m.ToolCallsTotal)
	tailer.WithActivity(mgr.onTailerActivity)
	tailerCtx, cancel := context.WithCancel(ctx) //nolint:gosec // cancel is stored in mgr.tailerCancel and invoked by Shutdown/path-change restart
	mgr.mu.Lock()
	mgr.tailer = tailer
	mgr.tailerParent = ctx
	mgr.tailerCtx = tailerCtx
	mgr.tailerCancel = cancel
	mgr.mu.Unlock()
}

func (mgr *Manager) currentTurnID() string {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return mgr.current
}

// onTailerActivity is the transcript Tailer's per-turn liveness hook. For each
// agent_stream event of the in-flight turn it advances LastActivityAt and resets
// the turn deadline, turning the AfterFunc(TurnTimeout) into an inactivity timer:
// a turn that keeps streaming runs as long as it makes progress, while a silent
// (hung) turn still fails after TurnTimeout of no transcript output. Events for a
// stale or already-cleared turn are ignored so a late event cannot extend or
// resurrect a turn the timeout path has finished.
func (mgr *Manager) onTailerActivity(turnID string) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if turnID == "" || turnID != mgr.current || mgr.timer == nil {
		return
	}
	mgr.store.Touch(turnID, mgr.now())
	mgr.timer.Reset(mgr.cfg.TurnTimeout)
}

// logTranscriptEnabled returns true unless CCW_LOG_TRANSCRIPT is explicitly "false".
func logTranscriptEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("CCW_LOG_TRANSCRIPT")))
	return v != "false"
}

// secretsFromEnv collects secret values from the process environment whose key
// matches common secret-bearing patterns. Values shorter than 8 chars are
// filtered by the Redactor itself.
func secretsFromEnv() map[string]string {
	patterns := []string{"_TOKEN", "_SECRET", "_KEY", "_PASSWORD"}
	explicit := []string{
		"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN",
		"OPENAI_API_KEY", "GIT_TOKEN",
	}
	out := make(map[string]string)
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		k, v := kv[:eq], kv[eq+1:]
		ku := strings.ToUpper(k)
		for _, pat := range patterns {
			if strings.HasSuffix(ku, pat) {
				out[k] = v
				break
			}
		}
	}
	for _, k := range explicit {
		if v, ok := os.LookupEnv(k); ok {
			out[k] = v
		}
	}
	return out
}

// SetWriterForTest injects a writer and marks the session READY. Test-only.
func (mgr *Manager) SetWriterForTest(w ptyWriter) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	mgr.w = w
	mgr.state = Ready
}

// SimulateExitForTest kills the injected fakeProc so the real watch() goroutine
// fires. Callers must have used InjectProcForTest first. Test-only.
//
// Deprecated: use InjectProcForTest + proc.kill() directly, which exercises the
// real watch() path. This shim exists only for callers not yet migrated.
func (mgr *Manager) SimulateExitForTest(err error) {
	mgr.mu.Lock()
	proc := mgr.proc
	// Mark stopping=true so watch() does not attempt a relaunch (the caller
	// expects the old "fail immediately" semantics).
	mgr.stopping = true
	mgr.mu.Unlock()
	if proc != nil {
		_ = proc.Close()
	}
}

// FireActivityForTest drives the transcript activity hook directly, simulating a
// tailer-observed agent_stream event for turnID. Test-only: exercises the
// inactivity-timer reset without standing up a real tailer goroutine.
func (mgr *Manager) FireActivityForTest(turnID string) {
	mgr.onTailerActivity(turnID)
}

// SetSpawnForTest replaces the spawn function used by Start/relaunch. Test-only.
func (mgr *Manager) SetSpawnForTest(fn func(cfg Config, resume bool) (ClaudeProcess, error)) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	mgr.spawn = fn
}

// InjectProcForTest injects a fake process as the running proc, marks the
// session Ready, and starts the watch goroutine. Test-only.
func (mgr *Manager) InjectProcForTest(proc ClaudeProcess) {
	mgr.mu.Lock()
	mgr.proc, mgr.w = proc, proc
	mgr.state = Ready
	mgr.mu.Unlock()
	mgr.wg.Add(2)
	go mgr.readPTY(proc)
	go mgr.watch(proc)
}

// SetTranscriptPathForTest sets the persisted transcript path the recovery path
// reads at resume time. Test-only: in production this is set by a prior turn's
// Stop hook (the session JSONL accumulates across turns).
func (mgr *Manager) SetTranscriptPathForTest(path string) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	mgr.transcriptPath = path
}

// ShouldResumeForTest exposes shouldResume to the external session_test
// package so the boot-crash resume invariant can be asserted directly. Test-only.
func (mgr *Manager) ShouldResumeForTest() bool { return mgr.shouldResume() }

// RingContainsForTest reports whether the ring buffer currently holds needle.
// Test-only; lets relaunch tests assert the ring was reset.
func (mgr *Manager) RingContainsForTest(needle string) bool {
	return mgr.ring.contains(needle)
}

// SetStoppingForTest marks the session as stopping (as if Shutdown was called).
// Test-only.
func (mgr *Manager) SetStoppingForTest() {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	mgr.stopping = true
}

// SetBootingForTest transitions the session state to Booting while leaving the
// in-flight turn id intact. Test-only: simulates the Dead->Booting window during
// crash recovery so the Complete guard can be exercised in isolation.
func (mgr *Manager) SetBootingForTest() {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	mgr.state = Booting
}

// SetDeadForTest transitions the session state to Dead while leaving the
// in-flight turn id intact. Test-only: simulates the crash window before relaunch
// so the Complete Dead-state guard can be exercised without a real watch goroutine.
func (mgr *Manager) SetDeadForTest() {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	mgr.state = Dead
}

// Start spawns claude, reads its PTY into the ring buffer, navigates the boot
// dialogs, and marks READY once the TUI settles.
func (mgr *Manager) Start(ctx context.Context) error {
	proc, err := mgr.spawn(mgr.cfg, false)
	if err != nil {
		return err
	}
	mgr.mu.Lock()
	mgr.proc, mgr.w = proc, proc
	mgr.mu.Unlock()

	mgr.wg.Add(2)
	go mgr.readPTY(proc)
	go mgr.watch(proc)

	mgr.bootWait(proc)
	return nil
}

// readPTY copies the interactive TUI's output into the ring buffer. It is the
// only window into the session: bootWait reads it for dialogs and watch logs
// its tail on exit.
func (mgr *Manager) readPTY(proc claudeProcess) {
	defer mgr.wg.Done()
	buf := make([]byte, 4096)
	for {
		n, err := proc.Read(buf)
		if n > 0 {
			_, _ = mgr.ring.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// bootWait accepts the (non-seedable) "Bypass Permissions" warning dialog that
// claude shows on every boot, then waits for the TUI to quiesce before marking
// the session READY. Without accepting the warning, the first turn's submit
// keystroke lands on the dialog and exits claude. Sequence confirmed against
// the real binary in docs/spike-findings.md. Uses real wall-clock (not the
// injected clock) since it polls live output. Respects mgr's lifecycle context
// so a shutdown or superseded relaunch aborts the wait promptly (finding 4).
func (mgr *Manager) bootWait(proc claudeProcess) {
	const (
		minBoot   = 4 * time.Second
		idleReady = 1500 * time.Millisecond
		poll      = 150 * time.Millisecond
	)
	// Capture the lifecycle context so we can select-abort on shutdown (finding 4).
	mgr.mu.Lock()
	lifecycleCtx := mgr.tailerParent // nil when StartTailer was not called
	mgr.mu.Unlock()

	start := time.Now()
	deadline := start.Add(mgr.cfg.BootTimeout)
	lastWritten := mgr.ring.written()
	lastChange := start
	acceptedBypass := false
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		// Bail if the session died or a newer relaunch superseded this proc, so
		// a stale bootWait does not poll the shared ring for the full timeout.
		if mgr.isDead() || !mgr.isActiveProc(proc) {
			return
		}
		// Abort promptly on shutdown/context cancellation (finding 4).
		if lifecycleCtx != nil && lifecycleCtx.Err() != nil {
			return
		}
		now := time.Now()
		if !now.Before(deadline) {
			break
		}
		if !acceptedBypass && mgr.ring.contains("Bypass Permissions mode") {
			time.Sleep(600 * time.Millisecond)
			mgr.writeRaw("\x1b[B") // down arrow -> "Yes, I accept"
			time.Sleep(250 * time.Millisecond)
			mgr.writeRaw("\r")
			acceptedBypass = true
			mgr.log.Info("accepted bypass-permissions warning")
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if w := mgr.ring.written(); w != lastWritten {
			lastWritten = w
			lastChange = now
		}
		if now.Sub(start) >= minBoot && now.Sub(lastChange) >= idleReady {
			break
		}
		// Use ticker instead of bare time.Sleep so the context check fires promptly.
		if lifecycleCtx != nil {
			select {
			case <-ticker.C:
			case <-lifecycleCtx.Done():
				return
			}
		} else {
			<-ticker.C
		}
	}
	mgr.mu.Lock()
	if mgr.state == Booting && mgr.proc == proc {
		mgr.state = Ready
	}
	mgr.mu.Unlock()
	mgr.log.Info("session ready", "boot_ms", time.Since(start).Milliseconds(), "accepted_bypass", acceptedBypass)
}

func (mgr *Manager) isActiveProc(proc claudeProcess) bool {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return mgr.proc == proc
}

func (mgr *Manager) isDead() bool {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return mgr.state == Dead
}

func (mgr *Manager) writeRaw(s string) {
	mgr.mu.Lock()
	w := mgr.w
	mgr.mu.Unlock()
	if w != nil {
		_, _ = w.Write([]byte(s))
	}
}

// watch blocks until the supervised claude process exits, then handles
// recovery: relaunch with --continue (up to MaxRestarts consecutive crashes),
// re-submit any in-flight turn, or fail the turn and mark Dead if budget
// exhausted.
func (mgr *Manager) watch(proc claudeProcess) {
	defer mgr.wg.Done()
	err := proc.Wait()
	mgr.mu.Lock()
	if mgr.stopping {
		mgr.state = Dead
		mgr.mu.Unlock()
		mgr.log.Info("claude stopped (shutdown)")
		return
	}
	inFlight := mgr.current
	// Stop the in-flight turn timer before relaunch so it cannot fire during
	// the Dead->Booting->Ready window and mis-fail a successfully resumed turn
	// (finding 2).
	if mgr.timer != nil {
		mgr.timer.Stop()
	}
	mgr.restarts++
	attempt := mgr.restarts
	mgr.state = Dead // brief: Submit rejects until relaunch flips to Ready
	mgr.mu.Unlock()

	mgr.log.Error("claude exited unexpectedly", "err", err, "in_flight_turn", inFlight,
		"attempt", attempt, "max_restarts", mgr.cfg.MaxRestarts, "pty_tail", mgr.ring.tail(800))

	if attempt > mgr.cfg.MaxRestarts {
		mgr.log.Error("claude restart budget exhausted; operator will respawn",
			"max_restarts", mgr.cfg.MaxRestarts)
		if inFlight != "" {
			mgr.failTurn(inFlight, fmt.Sprintf("claude died; restart budget (%d) exhausted", mgr.cfg.MaxRestarts))
		}
		return // state stays Dead
	}

	// Increment only on actual relaunch attempts (finding 4: must not count the
	// terminal death where no relaunch occurs).
	mgr.m.ClaudeRestarts.Inc()

	resumed, rerr := mgr.relaunch()
	if rerr != nil {
		mgr.mu.Lock()
		mgr.state = Dead
		mgr.mu.Unlock()
		mgr.log.Error("claude relaunch failed; operator will respawn", "err", rerr)
		if inFlight != "" {
			mgr.failTurn(inFlight, fmt.Sprintf("claude relaunch failed: %v", rerr))
		}
		return
	}
	mgr.log.Info("claude relaunched after exit", "attempt", attempt, "resumed_turn", inFlight, "resumed_conversation", resumed)
	if inFlight != "" {
		mgr.resumeTurn(inFlight, resumed)
	}
}

// relaunch spawns a fresh claude (with --continue when a conversation exists),
// rewires the PTY, restarts the reader+watcher, and waits for boot. The new
// watch goroutine handles the next death (restarts persists across relaunches).
// The returned bool reports whether an existing conversation was resumed
// (--continue); the caller (watch) threads it into resumeTurn so a fresh
// relaunch re-submits the original prompt instead of nudging a conversation
// that was never restored (finding 2).
func (mgr *Manager) relaunch() (resumed bool, err error) {
	resume := mgr.shouldResume()
	proc, err := mgr.spawn(mgr.cfg, resume)
	if err != nil {
		return false, err
	}
	mgr.mu.Lock()
	// Re-check stopping under the lock after spawn (finding 8): if Shutdown ran
	// while we were spawning, discard the freshly created proc and abort.
	if mgr.stopping {
		mgr.mu.Unlock()
		_ = proc.Close()
		return false, fmt.Errorf("session is shutting down")
	}
	old := mgr.proc
	mgr.proc, mgr.w = proc, proc
	mgr.state = Booting
	mgr.mu.Unlock()
	// Close the dead proc's PTY master so its fd does not leak across relaunches
	// (cmd.Wait reaps the child, but the master stays open otherwise).
	if old != nil {
		_ = old.Close()
	}
	// Clear the dead proc's output so bootWait navigates against only the new
	// proc's dialogs. old.Close() above causes the old ptmx Read to error, but
	// the old readPTY goroutine may still be inside ring.Write with a final buffer
	// when reset() runs. ring.mu serialises the two so there is no data race, but
	// a stale tail from the dead proc can land in the ring after the reset in a
	// small window. bootWait's minBoot delay (4s) absorbs this in practice.
	mgr.ring.reset()
	mgr.wg.Add(2)
	go mgr.readPTY(proc)
	go mgr.watch(proc)
	mgr.bootWait(proc) // flips Booting -> Ready
	// Fire the conversationRestart hook only when an existing conversation was
	// resumed (--continue); a fresh first-boot relaunch has nothing to restart.
	if resume && mgr.OnRestart != nil {
		mgr.OnRestart()
	}
	return resume, nil
}

// shouldResume reports whether a prior conversation exists to --continue.
// Invariant: resume only when a transcript was actually persisted (a turn has
// completed, a transcript path was recorded by a Stop hook, or a transcript
// JSONL already exists on disk). An in-flight turn (mgr.current != "") is NOT
// sufficient on its own: Submit() sets mgr.current the instant a turn is
// accepted, before claude has written anything to disk, so a crash in that
// window has no conversation to --continue into. Passing --continue there
// makes claude exit immediately ("No conversation found to continue"),
// burning the whole restart budget on an identical crash loop.
//
// The on-disk check covers a genuine mid-first-turn crash: claude did real
// work and wrote ~/.claude/projects/<dir>/<sid>.jsonl, but the crash itself
// killed the Stop hook before it could POST transcriptPath back. Without this
// check that shape would relaunch fresh and discard a resumable conversation,
// which is worse than the boot-crash bug this function was hardened against.
// --continue's own precondition is exactly "does a transcript exist on disk",
// so checking it directly here keeps shouldResume in lockstep with it.
func (mgr *Manager) shouldResume() bool {
	mgr.mu.Lock()
	turnsCompleted := mgr.turnsCompleted
	transcriptPath := mgr.transcriptPath
	mgr.mu.Unlock()
	if turnsCompleted > 0 || transcriptPath != "" {
		return true
	}
	return mgr.transcriptExistsOnDisk()
}

// transcriptExistsOnDisk reports whether claude has any transcript on disk in
// this pod's transcript directory (~/.claude/projects/<ProjectDirName(Workspace)>,
// computed via convstore.TranscriptDir). It only checks for *presence* of any
// *.jsonl, NOT a specific session id: at the point shouldResume runs (a crash
// relaunch, never initial boot), --continue resumes "the most recent
// conversation in the workspace" and there is no reliable session id to scope
// to for the target case (a mid-first-turn crash whose Stop hook never fired,
// so mgr.currentSessionID is still ""). Correctness rests on the one-pod =
// one-conversation invariant: this pod owns its filesystem and writes at most
// one conversation into this dir (its own, plus at most one Restore/Fork blob
// at boot which IS this pod's conversation). A stray extra .jsonl would let
// --continue pick a different conversation than the crashed turn; that is not
// reachable in the current architecture but is not enforced here, so the match
// count is logged for observability. Returns false (never resume) when HomeDir
// or Workspace is unset, since TranscriptDir would be meaningless, and on any
// glob error - this can only add resumes, never weaken the boot-crash-loop
// protection.
func (mgr *Manager) transcriptExistsOnDisk() bool {
	if mgr.cfg.HomeDir == "" || mgr.cfg.Workspace == "" {
		return false
	}
	dir := convstore.TranscriptDir(mgr.cfg.HomeDir, mgr.cfg.Workspace)
	matches, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		mgr.log.Warn("resume: transcript glob failed; treating as no on-disk transcript",
			"action", "should_resume", "dir", dir, "err", err)
		return false
	}
	if len(matches) == 0 {
		return false
	}
	mgr.log.Info("resume: on-disk transcript present; relaunch will --continue",
		"action", "should_resume", "dir", dir, "jsonl_count", len(matches))
	return true
}

// resumeTurn restores the in-flight turn after a crash+relaunch. resumed
// reports whether relaunch actually resumed an existing conversation
// (--continue); it must never be assumed true just because a turn was in
// flight (that was the original boot-crash bug).
//
//   - resumed==false (fresh relaunch, no --continue): nothing was restored,
//     so there is no conversation for a bare submit keystroke to land in.
//     Re-send the turn's original prompt in full (paste+submit) from the
//     store so claude actually runs it, instead of wedging Busy until
//     TurnTimeout on an empty conversation.
//   - resumed==true: --continue has restored the full conversation to disk,
//     so resumeTurn reads the last message of the restored transcript to
//     decide what the crash interrupted:
//   - Turn still in flight (last message is the user prompt, a tool_result,
//     or an assistant tool_use awaiting a tool) -> nudge: write a plain
//     submit keystroke so claude picks up where it left off. The original
//     prompt is NOT re-pasted (--continue already restored it; re-pasting
//     would double-submit).
//   - Turn already finished before the crash (last message is a terminal
//     assistant answer, Stop hook never landed) -> complete from
//     transcript: do NOT submit. A bare submit here injects an empty user
//     turn and triggers duplicate work (hazard 1); leaving the turn
//     unresolved wedges it Busy until TurnTimeout (hazard 2). Instead
//     synthesize the result from the restored transcript so the operator
//     gets the real output and the turn resolves now.
//
// Any transcript read error or ambiguous shape falls through to the nudge so
// resume is never worse than the pre-transcript behavior. No-op if the turn was
// resolved during relaunch.
func (mgr *Manager) resumeTurn(id string, resumed bool) {
	mgr.mu.Lock()
	if mgr.current != id || mgr.state != Ready {
		mgr.mu.Unlock()
		return
	}
	if mgr.w == nil {
		mgr.mu.Unlock()
		return
	}
	// Capture w and seq under the lock, then release before the write.
	// This mirrors the Submit/Interject lock discipline: PTY writes happen
	// outside the lock so a slow/wedged PTY during crash recovery cannot block
	// Snapshot, Alive, or Complete (finding 3).
	w := mgr.w
	seq := mgr.cfg.SubmitSeq
	started := mgr.currentStarted
	path := mgr.transcriptPath
	mgr.mu.Unlock()

	if !resumed {
		// Fresh relaunch: --continue restored nothing. A bare nudge here would
		// land in an empty conversation and never actually re-run the turn.
		mgr.resubmitOriginalPrompt(id, w, seq, started)
		return
	}

	// Decide nudge vs complete-from-transcript by reading the restored transcript.
	// tool_use is a mid-turn stop_reason (claude paused to run a tool), so it stays
	// in-flight and falls through to the nudge; only a terminal assistant answer
	// means the turn finished before the crash.
	if path != "" {
		role, stop, err := transcript.LastMessage(path)
		if err == nil && role == "assistant" && stop != "" && stop != "tool_use" {
			if finalText, usage, sr, rerr := transcript.LastAssistant(path); rerr == nil {
				mgr.completeFromTranscript(id, started, path, finalText, usage, sr)
				return
			} else {
				mgr.log.Warn("resume: turn looked complete but transcript read failed; nudging",
					"action", "turn_resume", "turn_id", id, "err", rerr)
			}
		}
	}

	// In-flight turn: send only the submit keystroke - the prompt is already in the
	// restored conversation via --continue.
	if _, err := w.Write([]byte(seq.Submit)); err != nil {
		mgr.m.TurnResumes.WithLabelValues("write_fail", "nudge").Inc()
		mgr.failTurn(id, fmt.Sprintf("resume write submit: %v", err))
		return
	}
	mgr.installResumeTimer(id, started, "nudge")
}

// resubmitOriginalPrompt re-sends a turn's original prompt in full
// (paste+submit) after a fresh relaunch (no --continue): nothing was restored,
// so the bare-nudge sequence has no conversation to land in and the crashed
// turn would otherwise never actually re-run. text comes back from the store
// (the same prompt Submit originally wrote), and the write mirrors Submit's
// own paste+SubmitDelay+submit sequence. Falls back to a bare nudge only if
// the store has no record or empty text for id (should not happen in
// practice; keeps resume no worse than a plain nudge).
func (mgr *Manager) resubmitOriginalPrompt(id string, w ptyWriter, seq SubmitSequence, started time.Time) {
	rec, ok := mgr.store.Get(id)
	if !ok || rec.Text == "" {
		mgr.log.Error("resume: turn text unavailable for fresh-relaunch resubmit; falling back to nudge",
			"action", "turn_resume", "turn_id", id)
		if _, err := w.Write([]byte(seq.Submit)); err != nil {
			mgr.m.TurnResumes.WithLabelValues("write_fail", "nudge").Inc()
			mgr.failTurn(id, fmt.Sprintf("resume write submit: %v", err))
			return
		}
		mgr.installResumeTimer(id, started, "nudge")
		return
	}
	if _, err := w.Write([]byte(seq.PasteStart + rec.Text + seq.PasteEnd)); err != nil {
		mgr.m.TurnResumes.WithLabelValues("write_fail", "resubmit").Inc()
		mgr.failTurn(id, fmt.Sprintf("resume write paste: %v", err))
		return
	}
	time.Sleep(mgr.cfg.SubmitDelay)
	if _, err := w.Write([]byte(seq.Submit)); err != nil {
		mgr.m.TurnResumes.WithLabelValues("write_fail", "resubmit").Inc()
		mgr.failTurn(id, fmt.Sprintf("resume write submit: %v", err))
		return
	}
	mgr.installResumeTimer(id, started, "resubmit")
}

// installResumeTimer re-acquires the lock to install a fresh turn timer and
// flip state back to Busy after a successful resume write (nudge or
// resubmit), then records the outcome. Re-checks mgr.current in case a
// concurrent failTurn/Complete cleared it during the write (no-op if so).
func (mgr *Manager) installResumeTimer(id string, started time.Time, mode string) {
	mgr.mu.Lock()
	if mgr.current != id {
		mgr.mu.Unlock()
		return
	}
	// Install a fresh timer so the resumed turn gets a full TurnTimeout budget.
	// The old timer was stopped by watch() before relaunch. The timer is a
	// relative AfterFunc, so the fresh budget needs no separate anchor field;
	// crucially we do NOT touch currentStarted, so TurnDuration still reflects
	// the full wall-clock including pre-crash time (audit finding 3).
	mgr.timer = time.AfterFunc(mgr.cfg.TurnTimeout, func() { mgr.failTimeout(id) })
	mgr.state = Busy
	now := mgr.now()
	mgr.mu.Unlock()
	mgr.m.TurnResumes.WithLabelValues("ok", mode).Inc()
	mgr.log.Info("resumed in-flight turn after relaunch", "action", "turn_resume", "turn_id", id,
		"resume_mode", mode, "duration_ms", now.Sub(started).Milliseconds())
}

// completeFromTranscript resolves an in-flight turn whose output claude finished
// before the crash, using the restored transcript instead of a Stop hook (which
// never landed). It mirrors Complete's finalize path - store.Complete, success
// metrics, token metering, fireDone - so the operator gets the real result and
// no empty nudge triggers duplicate work (hazard 1); clearing the turn also stops
// it sitting Busy until TurnTimeout (hazard 2). finalText/usage/stopReason are the
// already-read last-assistant fields; per-turn token totals are summed here.
// No-op if the turn was resolved concurrently during relaunch.
func (mgr *Manager) completeFromTranscript(id string, started time.Time, path, finalText string, usage json.RawMessage, stopReason string) {
	var tokens []TurnTokens
	if tt, err := transcript.SumTurnTokens(path); err == nil {
		tokens = tt
	} else {
		mgr.log.Warn("resume: token sum failed; completing without token metrics",
			"action", "turn_complete", "turn_id", id, "err", err)
	}

	mgr.mu.Lock()
	if mgr.current != id {
		mgr.mu.Unlock()
		return
	}
	if mgr.timer != nil {
		mgr.timer.Stop()
	}
	now := mgr.now()
	if err := mgr.store.Complete(id, finalText, nil, usage, stopReason, now); err != nil {
		// Record not found means a correlation bug; do not bump success metrics or
		// fire a phantom completion (mirrors Complete, finding 6).
		mgr.mu.Unlock()
		mgr.log.Error("store.Complete returned error in completeFromTranscript; skipping metrics and callback",
			"action", "turn_complete", "turn_id", id, "err", err)
		return
	}
	mgr.clearCurrentLocked(Ready)
	mgr.turnsSucceeded++
	mgr.restarts = 0          // a completed turn proves the session healthy
	mgr.currentSessionID = "" // clear for next turn
	mgr.m.TurnsTotal.WithLabelValues("complete").Inc()
	mgr.m.TurnDuration.Observe(now.Sub(started).Seconds())
	rec, _ := mgr.store.Get(id)
	mgr.mu.Unlock()

	// Cost (result.json) is owned by the Stop hook and not read on this path; only
	// token totals from the transcript are metered.
	mgr.meterTokens(HookResult{Usage: usage, TurnTokens: tokens})
	mgr.m.TurnResumes.WithLabelValues("ok", "complete_from_transcript").Inc()
	mgr.log.Info("resumed turn completed from transcript", "action", "turn_complete", "turn_id", id,
		"resume_mode", "complete_from_transcript", "stop_reason", stopReason,
		"duration_ms", now.Sub(started).Milliseconds())
	mgr.fireDone(rec)
}

// failTurn fails the in-flight turn immediately (used when claude died and the
// restart budget is exhausted or a relaunch failed). Marks the session Dead so
// the operator respawns a fresh pod. No-op if id is no longer current.
func (mgr *Manager) failTurn(id, reason string) {
	mgr.mu.Lock()
	if mgr.current != id {
		mgr.mu.Unlock()
		return
	}
	if mgr.timer != nil {
		mgr.timer.Stop()
	}
	now := mgr.now()
	started := mgr.currentStarted
	if err := mgr.store.Fail(id, reason, now); err != nil {
		// Correlation bug: the record is missing. Do not bump metrics or fire
		// a phantom callback (finding 6).
		mgr.mu.Unlock()
		mgr.log.Error("store.Fail returned error in failTurn; skipping metrics and callback",
			"action", "turn_fail", "turn_id", id, "reason", reason, "err", err)
		return
	}
	mgr.clearCurrentLocked(Dead)
	mgr.currentSessionID = ""
	mgr.m.TurnsTotal.WithLabelValues("failed").Inc()
	mgr.m.TurnDuration.Observe(now.Sub(started).Seconds())
	rec, _ := mgr.store.Get(id)
	mgr.mu.Unlock()
	mgr.log.Warn("turn failed", "action", "turn_fail", "turn_id", id, "reason", reason, "duration_ms", now.Sub(started).Milliseconds())
	mgr.fireDone(rec)
}

func (mgr *Manager) Submit(text, callbackURL string) (string, error) {
	mgr.mu.Lock()
	if mgr.state == Dead {
		mgr.mu.Unlock()
		return "", fmt.Errorf("session dead")
	}
	if mgr.state == Booting {
		mgr.mu.Unlock()
		return "", fmt.Errorf("session not ready")
	}
	if mgr.current != "" {
		mgr.mu.Unlock()
		return "", ErrBusy
	}
	id := mgr.newID()
	now := mgr.now()
	mgr.store.Create(id, text, callbackURL, now)
	// Reserve the turn slot immediately so a concurrent Submit sees ErrBusy even
	// while we are sleeping between the paste and the submit keystroke.
	mgr.current, mgr.currentStarted, mgr.state = id, now, Busy
	mgr.m.TurnInFlight.Set(1)
	w := mgr.w
	seq := mgr.cfg.SubmitSeq
	mgr.mu.Unlock()

	// Paste and submit happen outside the lock so that Snapshot/Alive/Complete
	// are not blocked for the full SubmitDelay (mirrors the Interject fix,
	// finding 2). Both writes are to the same logical PTY, ordered by the fact
	// that only one goroutine (this one) drives the paste+submit sequence at a time
	// (the turn slot above guarantees no concurrent Submit).
	if _, err := w.Write([]byte(seq.PasteStart + text + seq.PasteEnd)); err != nil {
		mgr.failSubmitWrite(id, "write pty paste", err, now)
		return "", fmt.Errorf("write pty paste: %w", err)
	}
	time.Sleep(mgr.cfg.SubmitDelay)
	if _, err := w.Write([]byte(seq.Submit)); err != nil {
		mgr.failSubmitWrite(id, "write pty submit", err, now)
		return "", fmt.Errorf("write pty submit: %w", err)
	}

	// Install the timeout timer and log only after writes succeed.
	mgr.mu.Lock()
	mgr.timer = time.AfterFunc(mgr.cfg.TurnTimeout, func() { mgr.failTimeout(id) })
	mgr.mu.Unlock()
	mgr.log.Info("turn submitted", "action", "turn_submit", "turn_id", id)
	return id, nil
}

// Interject types `text` into the live claude session while a turn is already
// in flight, exactly as a user adding new context mid-session would. It reuses
// the paste+submit keystroke sequence but creates NO turn record and does not
// touch the current turn id, state, or timeout: the running turn absorbs the
// input and still completes with a single Stop hook. Returns ErrNotBusy when no
// turn is in flight (callers should Submit a fresh turn instead).
//
// The lock is released before the SubmitDelay sleep so that concurrent callers
// (Complete, Snapshot, failTimeout) are not blocked during the deliberate
// keystroke pause (finding 9).
func (mgr *Manager) Interject(text string) error {
	mgr.mu.Lock()
	switch mgr.state {
	case Dead:
		mgr.mu.Unlock()
		return fmt.Errorf("session dead")
	case Booting:
		mgr.mu.Unlock()
		return fmt.Errorf("session not ready")
	}
	if mgr.current == "" {
		mgr.mu.Unlock()
		return ErrNotBusy
	}
	turnID := mgr.current
	w := mgr.w
	seq := mgr.cfg.SubmitSeq
	mgr.mu.Unlock()

	// Writes and sleep happen outside the lock (finding 9).
	if _, err := w.Write([]byte(seq.PasteStart + text + seq.PasteEnd)); err != nil {
		return fmt.Errorf("write pty paste: %w", err)
	}
	time.Sleep(mgr.cfg.SubmitDelay)
	if _, err := w.Write([]byte(seq.Submit)); err != nil {
		return fmt.Errorf("write pty submit: %w", err)
	}
	mgr.m.Interjections.Inc()
	mgr.log.Info("turn interjection", "action", "interject", "turn_id", turnID)
	return nil
}

// Complete is invoked from the internal endpoint when a Stop hook fires.
func (mgr *Manager) Complete(r HookResult) error {
	mgr.mu.Lock()
	// Every delivery is a received hook regardless of outcome (finding 5).
	mgr.m.HookReceived.Inc()
	id := mgr.current
	if id == "" {
		mgr.m.HookOutcome.WithLabelValues("no_turn").Inc()
		mgr.mu.Unlock()
		return fmt.Errorf("no in-flight turn")
	}
	// Reject hooks that arrive during crash recovery (Dead or Booting). The hook
	// is from the dead process; the genuine post-resume completion only arrives
	// after resumeTurn flips state back to Busy (finding 1).
	if mgr.state == Dead || mgr.state == Booting {
		mgr.m.HookOutcome.WithLabelValues("rejected").Inc()
		mgr.mu.Unlock()
		mgr.log.Warn("hook during recovery; rejecting", "turn_id", id, "state", mgr.state)
		return fmt.Errorf("hook during recovery: session %s", mgr.state)
	}
	// Reject stale/duplicate hooks: if this is not the first hook for this turn
	// and the session already has a recorded SessionID that differs, the hook
	// is for a previously completed turn or a dead process (finding 3).
	if r.SessionID != "" && mgr.currentSessionID != "" && r.SessionID != mgr.currentSessionID {
		mgr.m.HookOutcome.WithLabelValues("rejected").Inc()
		mgr.mu.Unlock()
		mgr.log.Warn("hook session id mismatch; rejecting stale hook",
			"turn_id", id, "expected_session_id", mgr.currentSessionID, "got_session_id", r.SessionID)
		return fmt.Errorf("hook session id mismatch: stale or duplicate hook")
	}
	// Record the SessionID on first hook so subsequent duplicates can be detected.
	if r.SessionID != "" && mgr.currentSessionID == "" {
		mgr.currentSessionID = r.SessionID
	}
	if mgr.timer != nil {
		mgr.timer.Stop()
	}
	now := mgr.now()
	started := mgr.currentStarted // capture before clearCurrentLocked resets state
	if r.TranscriptPath != "" {
		prevPath := mgr.transcriptPath
		mgr.transcriptPath = r.TranscriptPath
		if mgr.tailer != nil {
			pathChanged := r.TranscriptPath != prevPath
			if !mgr.tailerStarted {
				// First start: launch tailer on the new path.
				mgr.tailerStarted = true
				path := mgr.transcriptPath
				tailer := mgr.tailer
				tailerCtx := mgr.tailerCtx
				mgr.wg.Add(1)
				go func() {
					defer mgr.wg.Done()
					if err := tailer.Follow(tailerCtx, path); err != nil && tailerCtx.Err() == nil {
						mgr.log.Error("transcript tailer error", "err", err)
					}
				}()
			} else if pathChanged && mgr.tailerParent != nil && mgr.tailerParent.Err() == nil {
				// Transcript path changed after a crash+relaunch: cancel the old Follow
				// and start a fresh one on the new path so we do not tail a stale file.
				mgr.log.Warn("transcript path changed, restarting tailer",
					"old_path", prevPath, "new_path", r.TranscriptPath)
				mgr.tailerCancel()
				newCtx, newCancel := context.WithCancel(mgr.tailerParent) //nolint:gosec // newCancel is stored in mgr.tailerCancel and invoked by Shutdown/next path-change restart
				mgr.tailerCtx = newCtx
				mgr.tailerCancel = newCancel
				path := mgr.transcriptPath
				tailer := mgr.tailer
				mgr.wg.Add(1)
				go func() {
					defer mgr.wg.Done()
					if err := tailer.Follow(newCtx, path); err != nil && newCtx.Err() == nil {
						mgr.log.Error("transcript tailer error", "err", err)
					}
				}()
			} else if pathChanged {
				mgr.log.Warn("transcript path changed but session context is done; observability gap on new path",
					"old_path", prevPath, "new_path", r.TranscriptPath)
			}
		}
	}
	if err := mgr.store.Complete(id, r.FinalText, r.ResultJSON, r.Usage, r.StopReason, now); err != nil {
		// Record not found means a correlation bug (e.g. stale hook after store
		// reset). Do not bump success metrics or fire a phantom completion (finding 6).
		mgr.m.HookOutcome.WithLabelValues("store_error").Inc()
		mgr.mu.Unlock()
		mgr.log.Error("store.Complete returned error; skipping metrics and callback",
			"action", "turn_complete", "turn_id", id, "err", err)
		return fmt.Errorf("store Complete %s: %w", id, err)
	}
	mgr.clearCurrentLocked(Ready)
	mgr.turnsSucceeded++
	mgr.restarts = 0          // a completed turn proves the session healthy
	mgr.currentSessionID = "" // clear for next turn
	mgr.m.HookOutcome.WithLabelValues("ok").Inc()
	mgr.m.TurnsTotal.WithLabelValues("complete").Inc()
	mgr.m.TurnDuration.Observe(now.Sub(started).Seconds())
	rec, _ := mgr.store.Get(id)
	mgr.mu.Unlock()
	mgr.meterTokens(r)
	mgr.log.Info("turn complete", "action", "turn_complete", "turn_id", id, "duration_ms", now.Sub(started).Milliseconds())
	mgr.fireDone(rec)
	return nil
}

// meterTokens turns the already-captured per-turn usage into Prometheus
// counters. It runs after the lock is released (the counters are goroutine
// safe) and must never fail a turn: a missing or malformed field is logged and
// skipped, never propagated. Token counts come from the summed-per-turn
// TurnTokens (the last-message Usage undercounts agentic turns); cost comes from
// result.json's total_cost_usd when the agent wrote one.
func (mgr *Manager) meterTokens(r HookResult) {
	kind, repo, project := mgr.cfg.Kind, mgr.cfg.RepoName, mgr.cfg.Project
	for _, t := range r.TurnTokens {
		model := t.Model
		if model == "" {
			model = "unknown"
		}
		mgr.m.TurnTokensTotal.WithLabelValues("input", model, kind, repo, project).Add(float64(t.Input))
		mgr.m.TurnTokensTotal.WithLabelValues("output", model, kind, repo, project).Add(float64(t.Output))
		mgr.m.TurnTokensTotal.WithLabelValues("cache_read", model, kind, repo, project).Add(float64(t.CacheRead))
		mgr.m.TurnTokensTotal.WithLabelValues("cache_creation", model, kind, repo, project).Add(float64(t.CacheCreation))
	}
	if len(r.ResultJSON) > 0 {
		var rj struct {
			TotalCostUSD *float64 `json:"total_cost_usd"`
		}
		if err := json.Unmarshal(r.ResultJSON, &rj); err != nil {
			mgr.log.Warn("turn cost: malformed result.json, skipping", "err", err)
		} else if rj.TotalCostUSD != nil {
			mgr.m.TurnCostUSD.WithLabelValues(kind, repo, project).Add(*rj.TotalCostUSD)
		}
	}
}

// MeterTokensForTest exposes meterTokens to the external session_test package so
// the metric-label wiring can be asserted without driving a full turn.
func (mgr *Manager) MeterTokensForTest(r HookResult) { mgr.meterTokens(r) }

func (mgr *Manager) failTimeout(id string) {
	mgr.mu.Lock()
	if mgr.current != id {
		mgr.mu.Unlock()
		return
	}
	now := mgr.now()
	started := mgr.currentStarted
	if err := mgr.store.Fail(id, "turn timed out", now); err != nil {
		// Correlation bug: record missing. Do not bump metrics or fire a phantom
		// callback (finding 6).
		mgr.mu.Unlock()
		mgr.log.Error("store.Fail returned error in failTimeout; skipping metrics and callback",
			"action", "turn_timeout", "turn_id", id, "err", err)
		return
	}
	mgr.clearCurrentLocked(Ready)
	mgr.currentSessionID = ""
	mgr.m.TurnsTotal.WithLabelValues("failed").Inc()
	mgr.m.TurnDuration.Observe(now.Sub(started).Seconds())
	rec, _ := mgr.store.Get(id)
	mgr.mu.Unlock()
	mgr.log.Warn("turn timed out", "action", "turn_timeout", "turn_id", id, "duration_ms", now.Sub(started).Milliseconds())
	mgr.fireDone(rec)
}

func (mgr *Manager) clearCurrentLocked(next State) {
	mgr.current = ""
	mgr.turnsCompleted++ // all terminal turns (success + failed + timed-out)
	mgr.state = next
	mgr.m.TurnInFlight.Set(0)
}

// failSubmitWrite records a turn that failed during the Submit paste/submit
// write, after the slot was already reserved. It keeps ccw_turns_total
// consistent with turnsCompleted (incremented by clearCurrentLocked) so a
// write-failed turn is not invisible in the terminal-result metric.
func (mgr *Manager) failSubmitWrite(id, stage string, werr error, now time.Time) {
	mgr.mu.Lock()
	if mgr.current != id {
		// The turn was taken over by a concurrent code path (watch/resumeTurn/failTurn)
		// while Submit's PTY write was in progress outside the lock. Do not mutate
		// state or bump metrics: that path already owns the record (finding 1).
		mgr.mu.Unlock()
		return
	}
	started := mgr.currentStarted
	if err := mgr.store.Fail(id, fmt.Sprintf("%s: %v", stage, werr), now); err != nil {
		// Correlation bug: record is missing or already terminal. Skip metrics to
		// avoid phantom counts (matches failTurn/failTimeout behaviour).
		mgr.mu.Unlock()
		mgr.log.Error("store.Fail returned error in failSubmitWrite; skipping metrics",
			"action", "turn_fail", "turn_id", id, "stage", stage, "err", err)
		return
	}
	mgr.clearCurrentLocked(Ready)
	mgr.m.TurnsTotal.WithLabelValues("failed").Inc()
	mgr.m.TurnDuration.Observe(now.Sub(started).Seconds())
	mgr.mu.Unlock()
	mgr.log.Warn("turn submit write failed", "action", "turn_fail", "turn_id", id,
		"stage", stage, "err", werr, "duration_ms", now.Sub(started).Milliseconds())
}

func (mgr *Manager) fireDone(rec *turn.Record) {
	if mgr.OnTurnDone != nil && rec != nil {
		mgr.OnTurnDone(rec)
	}
}

func (mgr *Manager) Snapshot() Snapshot {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	var lastActivity time.Time
	if mgr.current != "" {
		if rec, ok := mgr.store.Get(mgr.current); ok {
			lastActivity = rec.LastActivityAt
		}
	}
	return Snapshot{
		State:          mgr.state,
		TurnsCompleted: mgr.turnsSucceeded, // successful turns only (finding 11)
		TurnsFinished:  mgr.turnsCompleted, // all terminal turns
		Model:          mgr.cfg.Model,
		Repo:           mgr.cfg.Repo,
		LastActivityAt: lastActivity,
	}
}

func (mgr *Manager) Alive() bool {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return mgr.state != Dead && mgr.state != Booting
}

func (mgr *Manager) TranscriptPath() string {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return mgr.transcriptPath
}

func (mgr *Manager) Shutdown(ctx context.Context) error {
	mgr.mu.Lock()
	w := mgr.w
	proc := mgr.proc
	mgr.stopping = true
	mgr.state = Dead
	cancel := mgr.tailerCancel
	mgr.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if w != nil {
		_, _ = w.Write([]byte("\x03")) // Ctrl-C
		_ = w.Close()
	}
	if proc != nil {
		// proc is claudeProcess; real *claudeProc has cmd.Process.Kill accessible
		// only via the concrete type. We close the PTY (done above via w.Close())
		// which causes the process to receive SIGHUP, completing the shutdown.
		// For real procs we also send SIGKILL via the concrete type assertion.
		if cp, ok := proc.(*claudeProc); ok {
			if err := cp.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				// A failed SIGKILL means the child may survive across pod restart.
				// Log at WARN so the operator knows to investigate (finding 6).
				mgr.log.Warn("shutdown: SIGKILL failed; child process may survive",
					"err", err)
			}
		}
	}
	// Wait for lifecycle goroutines (readPTY, watch, tailer Follow) to drain,
	// with a bounded timeout so Shutdown does not hang forever (finding 13).
	done := make(chan struct{})
	go func() { mgr.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(goroutineJoinTimeout):
		mgr.log.Warn("shutdown: goroutine join timed out", "timeout", goroutineJoinTimeout)
	case <-ctx.Done():
		// Context cancelled before goroutines drained: drain is incomplete.
		// Log so an aborted shutdown is observable and distinct from a clean join.
		mgr.log.Warn("shutdown: ctx cancelled before goroutines drained")
	}
	return nil
}
