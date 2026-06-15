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
	TurnsCompleted int    `json:"turnsCompleted"`
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

	mu             sync.Mutex
	w              ptyWriter
	proc           claudeProcess
	ring           *ringBuffer
	stopping       bool
	state          State
	current        string // in-flight turn id, "" when idle
	currentStarted time.Time
	timer          *time.Timer
	turnsCompleted int
	transcriptPath string
	restarts       int // consecutive crash-relaunches; reset on Complete

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
	tailerCtx, cancel := context.WithCancel(ctx)
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

// SimulateExitForTest drives the unexpected-exit path as if the supervised
// claude process had terminated with err. Test-only.
func (mgr *Manager) SimulateExitForTest(err error) { mgr.handleExit(err) }

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

	go mgr.readPTY(proc)
	go mgr.watch(proc)

	mgr.bootWait(proc)
	return nil
}

// readPTY copies the interactive TUI's output into the ring buffer. It is the
// only window into the session: bootWait reads it for dialogs and watch logs
// its tail on exit.
func (mgr *Manager) readPTY(proc claudeProcess) {
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
// injected clock) since it polls live output.
func (mgr *Manager) bootWait(proc claudeProcess) {
	const (
		minBoot   = 4 * time.Second
		idleReady = 1500 * time.Millisecond
		poll      = 150 * time.Millisecond
	)
	start := time.Now()
	deadline := start.Add(mgr.cfg.BootTimeout)
	lastWritten := mgr.ring.written()
	lastChange := start
	acceptedBypass := false
	for {
		// Bail if the session died or a newer relaunch superseded this proc, so
		// a stale bootWait does not poll the shared ring for the full timeout.
		if mgr.isDead() || !mgr.isActiveProc(proc) {
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
		time.Sleep(poll)
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
	err := proc.Wait()
	mgr.mu.Lock()
	if mgr.stopping {
		mgr.state = Dead
		mgr.mu.Unlock()
		mgr.log.Info("claude stopped (shutdown)")
		return
	}
	inFlight := mgr.current
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

// resumeTurn re-submits the in-flight turn's prompt to the restored session,
// keeping the same turn id (so the eventual Stop hook still correlates) and the
// original timeout timer. No-op if the turn was resolved during relaunch.
func (mgr *Manager) resumeTurn(id string) {
	mgr.mu.Lock()
	if mgr.current != id || mgr.state != Ready {
		mgr.mu.Unlock()
		return
	}
	rec, ok := mgr.store.Get(id)
	if !ok || mgr.w == nil {
		mgr.mu.Unlock()
		return
	}
	seq := mgr.cfg.SubmitSeq
	if _, err := mgr.w.Write([]byte(seq.PasteStart + rec.Text + seq.PasteEnd)); err != nil {
		mgr.mu.Unlock()
		mgr.failTurn(id, fmt.Sprintf("resume write paste: %v", err))
		return
	}
	time.Sleep(mgr.cfg.SubmitDelay)
	if _, err := mgr.w.Write([]byte(seq.Submit)); err != nil {
		mgr.mu.Unlock()
		mgr.failTurn(id, fmt.Sprintf("resume write submit: %v", err))
		return
	}
	mgr.state = Busy
	mgr.mu.Unlock()
	mgr.log.Info("resumed in-flight turn after relaunch", "turn_id", id)
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
	_ = mgr.store.Fail(id, reason, now)
	mgr.clearCurrentLocked(Dead)
	mgr.m.TurnsTotal.WithLabelValues("failed").Inc()
	mgr.m.TurnDuration.Observe(now.Sub(started).Seconds())
	rec, _ := mgr.store.Get(id)
	mgr.mu.Unlock()
	mgr.log.Warn("turn failed", "action", "turn_fail", "turn_id", id, "reason", reason, "duration_ms", now.Sub(started).Milliseconds())
	mgr.fireDone(rec)
}

// handleExit is kept for backwards compatibility with SimulateExitForTest.
// It drives the old "fail immediately on exit" path used by unit tests that
// test the legacy behaviour (prior to the restart loop). The new path goes
// through watch(). The test helper calls this directly, bypassing watch().
func (mgr *Manager) handleExit(err error) {
	mgr.mu.Lock()
	if mgr.stopping {
		mgr.state = Dead
		mgr.mu.Unlock()
		mgr.log.Info("claude stopped (shutdown)")
		return
	}
	id := mgr.current
	var rec *turn.Record
	var durMs int64
	if id != "" {
		if mgr.timer != nil {
			mgr.timer.Stop()
		}
		now := mgr.now()
		durMs = now.Sub(mgr.currentStarted).Milliseconds()
		_ = mgr.store.Fail(id, "claude exited", now)
		mgr.clearCurrentLocked(Ready) // sets state = Ready; overridden to Dead below
		mgr.m.TurnsTotal.WithLabelValues("failed").Inc()
		rec, _ = mgr.store.Get(id)
	}
	mgr.state = Dead
	mgr.mu.Unlock()

	mgr.m.ClaudeRestarts.Inc()
	if id != "" {
		mgr.log.Warn("turn failed: claude exited", "action", "turn_fail", "turn_id", id, "duration_ms", durMs)
	}
	mgr.log.Error("claude exited", "err", err, "pty_tail", mgr.ring.tail(800))
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
func (mgr *Manager) Interject(text string) error {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	switch mgr.state {
	case Dead:
		return fmt.Errorf("session dead")
	case Booting:
		return fmt.Errorf("session not ready")
	}
	if mgr.current == "" {
		return ErrNotBusy
	}
	seq := mgr.cfg.SubmitSeq
	if _, err := mgr.w.Write([]byte(seq.PasteStart + text + seq.PasteEnd)); err != nil {
		return fmt.Errorf("write pty paste: %w", err)
	}
	time.Sleep(mgr.cfg.SubmitDelay)
	if _, err := mgr.w.Write([]byte(seq.Submit)); err != nil {
		return fmt.Errorf("write pty submit: %w", err)
	}
	mgr.m.Interjections.Inc()
	mgr.log.Info("turn interjection", "action", "interject", "turn_id", mgr.current)
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
				go func() {
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
				newCtx, newCancel := context.WithCancel(mgr.tailerParent)
				mgr.tailerCtx = newCtx
				mgr.tailerCancel = newCancel
				path := mgr.transcriptPath
				tailer := mgr.tailer
				go func() {
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
	_ = mgr.store.Complete(id, r.FinalText, r.ResultJSON, r.Usage, r.StopReason, now)
	mgr.clearCurrentLocked(Ready)
	mgr.restarts = 0 // a completed turn proves the session healthy
	mgr.m.HookReceived.Inc()
	mgr.m.TurnsTotal.WithLabelValues("complete").Inc()
	mgr.m.TurnDuration.Observe(now.Sub(started).Seconds())
	if r.SessionID != "" {
		mgr.log.Debug("hook session id", "session_id", r.SessionID)
	}
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
	_ = mgr.store.Fail(id, "turn timed out", now)
	mgr.clearCurrentLocked(Ready)
	mgr.m.TurnsTotal.WithLabelValues("failed").Inc()
	mgr.m.TurnDuration.Observe(now.Sub(started).Seconds())
	rec, _ := mgr.store.Get(id)
	mgr.mu.Unlock()
	mgr.log.Warn("turn timed out", "action", "turn_timeout", "turn_id", id, "duration_ms", now.Sub(started).Milliseconds())
	mgr.fireDone(rec)
}

func (mgr *Manager) clearCurrentLocked(next State) {
	mgr.current = ""
	mgr.turnsCompleted++
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
	return Snapshot{State: mgr.state, TurnsCompleted: mgr.turnsCompleted, Model: mgr.cfg.Model, Repo: mgr.cfg.Repo}
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
	return nil
}
