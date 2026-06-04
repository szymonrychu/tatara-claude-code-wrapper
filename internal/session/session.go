// Package session supervises one interactive claude process over a PTY and
// turns API submissions into typed turns, correlating Stop-hook callbacks.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

func (s SubmitSequence) encode(text string) []byte {
	return []byte(s.PasteStart + text + s.PasteEnd + s.Submit)
}

var ErrBusy = errors.New("session busy")

type Config struct {
	ClaudePath  string
	Workspace   string
	Env         []string
	Model       string
	TurnTimeout time.Duration
	BootTimeout time.Duration
	SubmitSeq   SubmitSequence
}

// claudeArgs builds the interactive launch flags. Per the Task-1 spike, we
// pass NO --permission-mode / --dangerously-skip-permissions flag: bypass is
// configured via settings.defaultMode (bootstrap), and the flag would trigger
// an extra "Bypass Permissions" warning dialog. MCP comes from the cwd
// .mcp.json + enableAllProjectMcpServers, so no --mcp-config flag either.
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
	return &Manager{cfg: cfg, store: store, m: m, log: log, now: now, newID: newID, state: Booting}
}

// SetWriterForTest injects a writer and marks the session READY. Test-only.
func (mgr *Manager) SetWriterForTest(w ptyWriter) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	mgr.w = w
	mgr.state = Ready
}

// Start spawns claude, drains its PTY output, and marks READY after boot.
func (mgr *Manager) Start(ctx context.Context) error {
	proc, err := spawnClaude(mgr.cfg)
	if err != nil {
		return err
	}
	mgr.mu.Lock()
	mgr.proc, mgr.w = proc, proc
	mgr.mu.Unlock()

	go func() { _, _ = io.Copy(io.Discard, proc.ptmx) }() // drain TUI; debug ring buffer is a later enhancement
	go mgr.watch(proc)

	// Readiness: spike-confirmed heuristic. Provisional: bounded boot delay.
	time.Sleep(minDuration(2*time.Second, mgr.cfg.BootTimeout))
	mgr.mu.Lock()
	if mgr.state == Booting {
		mgr.state = Ready
	}
	mgr.mu.Unlock()
	mgr.log.Info("session ready")
	return nil
}

func (mgr *Manager) watch(proc *claudeProc) {
	err := proc.cmd.Wait()
	mgr.mu.Lock()
	mgr.state = Dead
	mgr.mu.Unlock()
	mgr.m.ClaudeRestarts.Inc()
	mgr.log.Error("claude exited", "err", err)
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
	if _, err := mgr.w.Write(mgr.cfg.SubmitSeq.encode(text)); err != nil {
		_ = mgr.store.Fail(id, fmt.Sprintf("write pty: %v", err), now)
		return "", fmt.Errorf("write pty: %w", err)
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

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
