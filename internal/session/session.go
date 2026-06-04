// Package session supervises one interactive claude process over a PTY and
// turns API submissions into typed turns, correlating Stop-hook callbacks.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

// DefaultSubmitSeq wraps a message in bracketed paste, then submits with CR.
// Confirmed/overridden by the Task-1 spike.
var DefaultSubmitSeq = SubmitSequence{PasteStart: "\x1b[200~", PasteEnd: "\x1b[201~", Submit: "\r"}

type SubmitSequence struct{ PasteStart, PasteEnd, Submit string }

var ErrBusy = errors.New("session busy")

type Config struct {
	ClaudePath  string
	Workspace   string
	Env         []string
	Model       string
	TurnTimeout time.Duration
	BootTimeout time.Duration
	SubmitDelay time.Duration // pause between the paste and the submit keystroke
	SubmitSeq   SubmitSequence
}

// claudeArgs builds the interactive launch flags. Per the Task-1 spike we pass
// NO permission flag: bypass is configured via settings.defaultMode
// (bootstrap). The "Bypass Permissions" warning dialog still appears at boot
// regardless and is accepted by bootWait. MCP comes from the cwd .mcp.json +
// enableAllProjectMcpServers, so no --mcp-config flag either.
func (c Config) claudeArgs() []string {
	args := []string{}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	return args
}

// HookResult is the payload cc-stop-hook POSTs to the internal endpoint.
type HookResult struct {
	SessionID  string          `json:"sessionId"`
	FinalText  string          `json:"finalText"`
	ResultJSON json.RawMessage `json:"resultJson,omitempty"`
	Usage      json.RawMessage `json:"usage,omitempty"`
	StopReason string          `json:"stopReason"`
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

	mu             sync.Mutex
	w              ptyWriter
	proc           *claudeProc
	ring           *ringBuffer
	state          State
	current        string // in-flight turn id, "" when idle
	currentStarted time.Time
	timer          *time.Timer
	turnsCompleted int
	transcriptPath string
}

func New(cfg Config, store *turn.Store, m *metrics.Metrics, log *slog.Logger, now func() time.Time, newID func() string) *Manager {
	if cfg.BootTimeout <= 0 {
		cfg.BootTimeout = 60 * time.Second
	}
	if cfg.SubmitDelay <= 0 {
		cfg.SubmitDelay = 400 * time.Millisecond
	}
	return &Manager{cfg: cfg, store: store, m: m, log: log, now: now, newID: newID, state: Booting, ring: newRing()}
}

// SetWriterForTest injects a writer and marks the session READY. Test-only.
func (mgr *Manager) SetWriterForTest(w ptyWriter) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	mgr.w = w
	mgr.state = Ready
}

// Start spawns claude, reads its PTY into the ring buffer, navigates the boot
// dialogs, and marks READY once the TUI settles.
func (mgr *Manager) Start(ctx context.Context) error {
	proc, err := spawnClaude(mgr.cfg)
	if err != nil {
		return err
	}
	mgr.mu.Lock()
	mgr.proc, mgr.w = proc, proc
	mgr.mu.Unlock()

	go mgr.readPTY(proc)
	go mgr.watch(proc)

	mgr.bootWait()
	return nil
}

// readPTY copies the interactive TUI's output into the ring buffer. It is the
// only window into the session: bootWait reads it for dialogs and watch logs
// its tail on exit.
func (mgr *Manager) readPTY(proc *claudeProc) {
	buf := make([]byte, 4096)
	for {
		n, err := proc.ptmx.Read(buf)
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
func (mgr *Manager) bootWait() {
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
		if mgr.isDead() {
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
	if mgr.state == Booting {
		mgr.state = Ready
	}
	mgr.mu.Unlock()
	mgr.log.Info("session ready", "boot_ms", time.Since(start).Milliseconds(), "accepted_bypass", acceptedBypass)
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

func (mgr *Manager) watch(proc *claudeProc) {
	err := proc.cmd.Wait()
	mgr.mu.Lock()
	mgr.state = Dead
	mgr.mu.Unlock()
	mgr.m.ClaudeRestarts.Inc()
	mgr.log.Error("claude exited", "err", err, "pty_tail", mgr.ring.tail(800))
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
		_ = mgr.store.Fail(id, fmt.Sprintf("write pty: %v", err), now)
		return "", fmt.Errorf("write pty paste: %w", err)
	}
	time.Sleep(mgr.cfg.SubmitDelay)
	if _, err := mgr.w.Write([]byte(seq.Submit)); err != nil {
		_ = mgr.store.Fail(id, fmt.Sprintf("write pty: %v", err), now)
		return "", fmt.Errorf("write pty submit: %w", err)
	}
	mgr.current, mgr.currentStarted, mgr.state = id, now, Busy
	mgr.m.TurnInFlight.Set(1)
	mgr.timer = time.AfterFunc(mgr.cfg.TurnTimeout, func() { mgr.failTimeout(id) })
	mgr.log.Info("turn submitted", "turn_id", id)
	return id, nil
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
	_ = mgr.store.Complete(id, r.FinalText, r.ResultJSON, r.Usage, r.StopReason, now)
	mgr.clearCurrentLocked()
	mgr.m.HookReceived.Inc()
	mgr.m.TurnsTotal.WithLabelValues("complete").Inc()
	mgr.m.TurnDuration.Observe(now.Sub(started).Seconds())
	if r.SessionID != "" {
		mgr.log.Debug("hook session id", "session_id", r.SessionID)
	}
	rec, _ := mgr.store.Get(id)
	mgr.mu.Unlock()
	mgr.log.Info("turn complete", "turn_id", id, "duration_ms", now.Sub(rec.StartedAt).Milliseconds())
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
	_ = mgr.store.Fail(id, "turn timed out", now)
	mgr.clearCurrentLocked()
	mgr.m.TurnsTotal.WithLabelValues("failed").Inc()
	rec, _ := mgr.store.Get(id)
	mgr.mu.Unlock()
	mgr.log.Warn("turn timed out", "turn_id", id)
	mgr.fireDone(rec)
}

func (mgr *Manager) clearCurrentLocked() {
	mgr.current = ""
	mgr.turnsCompleted++
	mgr.state = Ready
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
	return Snapshot{State: mgr.state, TurnsCompleted: mgr.turnsCompleted, Model: mgr.cfg.Model, Repo: ""}
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
	w, proc := mgr.w, mgr.proc
	mgr.state = Dead
	mgr.mu.Unlock()
	if w != nil {
		_, _ = w.Write([]byte("\x03")) // Ctrl-C
		_ = w.Close()
	}
	if proc != nil {
		_ = proc.cmd.Process.Kill()
	}
	return nil
}
