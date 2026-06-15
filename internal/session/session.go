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
	"strings"
	"sync"
	"time"

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
	ClaudePath  string
	Workspace   string
	Env         []string
	Model       string
	Repo        string // primary repository URL the pod is bound to ("" if none)
	TurnTimeout time.Duration
	BootTimeout time.Duration
	SubmitDelay time.Duration // pause between the paste and the submit keystroke
	SubmitSeq   SubmitSequence
	MaxRestarts int // crash-relaunch budget per session; default 3
}

// HookResult is the payload cc-stop-hook POSTs to the internal endpoint.
type HookResult struct {
	SessionID      string          `json:"sessionId"`
	FinalText      string          `json:"finalText"`
	ResultJSON     json.RawMessage `json:"resultJson,omitempty"`
	Usage          json.RawMessage `json:"usage,omitempty"`
	StopReason     string          `json:"stopReason"`
	TranscriptPath string          `json:"transcriptPath,omitempty"`
}

type State string

const (
	Booting State = "booting"
	Ready   State = "ready"
	Busy    State = "busy"
	Dead    State = "dead"
)

type Snapshot struct {
	State          State  `json:"state"`
	TurnsCompleted int    `json:"turnsCompleted"` // successful turns only (excludes failed/timed-out)
	TurnsFinished  int    `json:"turnsFinished"`  // all terminal turns (success + failed + timed-out)
	Model          string `json:"model"`
	Repo           string `json:"repo"`
}

type Manager struct {
	cfg   Config
	store *turn.Store
	m     *metrics.Metrics
	log   *slog.Logger
	now   func() time.Time
	newID func() string

	OnTurnDone func(*turn.Record)

	spawn func(cfg Config, resume bool) (claudeProcess, error)

	mu               sync.Mutex
	w                ptyWriter
	proc             claudeProcess
	ring             *ringBuffer
	stopping         bool
	state            State
	current          string // in-flight turn id, "" when idle
	currentStarted   time.Time
	currentSessionID string // claude's sessionId for the current turn (from hook); used for correlation
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

	mgr.m.ClaudeRestarts.Inc()
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

	if rerr := mgr.relaunch(); rerr != nil {
		mgr.mu.Lock()
		mgr.state = Dead
		mgr.mu.Unlock()
		mgr.log.Error("claude relaunch failed; operator will respawn", "err", rerr)
		if inFlight != "" {
			mgr.failTurn(inFlight, fmt.Sprintf("claude relaunch failed: %v", rerr))
		}
		return
	}
	mgr.log.Info("claude relaunched after exit", "attempt", attempt, "resumed_turn", inFlight)
	if inFlight != "" {
		mgr.resumeTurn(inFlight)
	}
}

// relaunch spawns a fresh claude (with --continue when a conversation exists),
// rewires the PTY, restarts the reader+watcher, and waits for boot. The new
// watch goroutine handles the next death (restarts persists across relaunches).
func (mgr *Manager) relaunch() error {
	proc, err := mgr.spawn(mgr.cfg, mgr.shouldResume())
	if err != nil {
		return err
	}
	mgr.mu.Lock()
	// Re-check stopping under the lock after spawn (finding 8): if Shutdown ran
	// while we were spawning, discard the freshly created proc and abort.
	if mgr.stopping {
		mgr.mu.Unlock()
		_ = proc.Close()
		return fmt.Errorf("session is shutting down")
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
	// proc's dialogs. The old proc's Wait() has returned and its PTY master is
	// closed, so its readPTY goroutine has stopped writing - this cannot race the
	// new proc's first bytes (readPTY(proc) starts below).
	mgr.ring.reset()
	mgr.wg.Add(2)
	go mgr.readPTY(proc)
	go mgr.watch(proc)
	mgr.bootWait(proc) // flips Booting -> Ready
	return nil
}

// shouldResume reports whether a prior conversation exists to --continue. A
// death during the very first boot (no turn ever submitted, none completed)
// relaunches fresh; anything later resumes.
func (mgr *Manager) shouldResume() bool {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return mgr.current != "" || mgr.turnsCompleted > 0 || mgr.transcriptPath != ""
}

// resumeTurn nudges the restored --continue session to continue the in-flight
// turn. It does NOT re-paste the original prompt: --continue already restored
// the full conversation including the user prompt, so re-pasting would
// double-submit and cause duplicate work (finding 1). Instead we send a plain
// submit keystroke (an empty "continue" nudge) so claude picks up where it
// left off. The same turn id is kept so the eventual Stop hook still correlates.
// A fresh timer replaces the stale one that was stopped by watch() before
// relaunch, giving the resumed turn a full uncontended TurnTimeout budget
// (finding 2). No-op if the turn was resolved during relaunch.
func (mgr *Manager) resumeTurn(id string) {
	mgr.mu.Lock()
	if mgr.current != id || mgr.state != Ready {
		mgr.mu.Unlock()
		return
	}
	if mgr.w == nil {
		mgr.mu.Unlock()
		return
	}
	seq := mgr.cfg.SubmitSeq
	// Send only the submit keystroke - the prompt is already in the restored
	// conversation via --continue (finding 1).
	if _, err := mgr.w.Write([]byte(seq.Submit)); err != nil {
		mgr.mu.Unlock()
		mgr.failTurn(id, fmt.Sprintf("resume write submit: %v", err))
		return
	}
	// Install a fresh timer so the resumed turn gets a full TurnTimeout budget
	// (finding 2). The old timer was stopped by watch() before relaunch.
	now := mgr.now()
	mgr.currentStarted = now
	mgr.timer = time.AfterFunc(mgr.cfg.TurnTimeout, func() { mgr.failTimeout(id) })
	mgr.state = Busy
	mgr.mu.Unlock()
	mgr.log.Info("resumed in-flight turn after relaunch", "action", "turn_resume", "turn_id", id)
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
	defer mgr.mu.Unlock()
	if mgr.state == Dead {
		return "", fmt.Errorf("session dead")
	}
	if mgr.state == Booting {
		return "", fmt.Errorf("session not ready")
	}
	if mgr.current != "" {
		return "", ErrBusy
	}
	id := mgr.newID()
	now := mgr.now()
	mgr.store.Create(id, text, callbackURL, now)
	// Two writes: paste the text, pause, then submit. A single concatenated
	// write does not submit reliably (spike). The pause is held under the lock
	// (turns are sequential, so nothing else is writing).
	seq := mgr.cfg.SubmitSeq
	if _, err := mgr.w.Write([]byte(seq.PasteStart + text + seq.PasteEnd)); err != nil {
		_ = mgr.store.Fail(id, fmt.Sprintf("write pty paste: %v", err), now)
		return "", fmt.Errorf("write pty paste: %w", err)
	}
	time.Sleep(mgr.cfg.SubmitDelay)
	if _, err := mgr.w.Write([]byte(seq.Submit)); err != nil {
		_ = mgr.store.Fail(id, fmt.Sprintf("write pty submit: %v", err), now)
		return "", fmt.Errorf("write pty submit: %w", err)
	}
	mgr.current, mgr.currentStarted, mgr.state = id, now, Busy
	mgr.m.TurnInFlight.Set(1)
	mgr.timer = time.AfterFunc(mgr.cfg.TurnTimeout, func() { mgr.failTimeout(id) })
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
	id := mgr.current
	if id == "" {
		mgr.mu.Unlock()
		return fmt.Errorf("no in-flight turn")
	}
	// Reject stale/duplicate hooks: if this is not the first hook for this turn
	// and the session already has a recorded SessionID that differs, the hook
	// is for a previously completed turn or a dead process (finding 3).
	if r.SessionID != "" && mgr.currentSessionID != "" && r.SessionID != mgr.currentSessionID {
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
		mgr.mu.Unlock()
		mgr.log.Error("store.Complete returned error; skipping metrics and callback",
			"action", "turn_complete", "turn_id", id, "err", err)
		return fmt.Errorf("store Complete %s: %w", id, err)
	}
	mgr.clearCurrentLocked(Ready)
	mgr.turnsSucceeded++
	mgr.restarts = 0          // a completed turn proves the session healthy
	mgr.currentSessionID = "" // clear for next turn
	mgr.m.HookReceived.Inc()
	mgr.m.TurnsTotal.WithLabelValues("complete").Inc()
	mgr.m.TurnDuration.Observe(now.Sub(started).Seconds())
	rec, _ := mgr.store.Get(id)
	mgr.mu.Unlock()
	mgr.log.Info("turn complete", "action", "turn_complete", "turn_id", id, "duration_ms", now.Sub(started).Milliseconds())
	mgr.fireDone(rec)
	return nil
}

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

func (mgr *Manager) fireDone(rec *turn.Record) {
	if mgr.OnTurnDone != nil && rec != nil {
		mgr.OnTurnDone(rec)
	}
}

func (mgr *Manager) Snapshot() Snapshot {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return Snapshot{
		State:          mgr.state,
		TurnsCompleted: mgr.turnsSucceeded, // successful turns only (finding 11)
		TurnsFinished:  mgr.turnsCompleted, // all terminal turns
		Model:          mgr.cfg.Model,
		Repo:           mgr.cfg.Repo,
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
			_ = cp.cmd.Process.Kill()
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
	}
	return nil
}
