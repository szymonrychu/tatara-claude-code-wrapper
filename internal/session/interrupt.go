package session

import (
	"errors"
	"fmt"
	"time"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/transcript"
)

// InterruptSeq is what an ESC keypress puts on the wire: one byte, no paste
// framing, no submit keystroke. It is deliberately NOT part of SubmitSequence -
// it is not a message, it cancels one.
const InterruptSeq = "\x1b"

// ErrInterruptUnavailable is returned when there is no PTY to write to (dead,
// booting, or shutting down). Like ErrProbeUnavailable it maps to 503, never
// 409: "a turn is in flight" is the only state in which interrupting makes
// sense.
var ErrInterruptUnavailable = errors.New("interrupt unavailable: no live session")

const (
	// interruptPollInterval is how often awaitInterruptedTurn re-reads the
	// transcript looking for the interruption marker. The spike measured the
	// ESC landing in ~40ms, so this is already an order of magnitude slower
	// than the event it waits for.
	interruptPollInterval = 200 * time.Millisecond
	// interruptSettleTimeout bounds that wait. Past it the ESC is treated as
	// not having taken effect and the turn is LEFT ALONE - resolving a turn
	// that is still running would hand the operator a completion for work
	// still in progress, which is worse than an unresolved turn it can still
	// see in Snapshot.
	interruptSettleTimeout = 30 * time.Second
)

// Interrupt writes a single ESC into the PTY, cancelling whatever the agent is
// doing, and returns the id of the turn that was in flight ("" when idle).
//
// ESC is the only mechanism that stops a turn WITHOUT killing the pod. The
// spike verified on CLI 2.1.201 and 2.1.226 that it interrupts synchronously in
// ~40ms and, crucially, DURING a running tool call - unlike pasted text, which
// is only consumed at the next tool-call boundary and is therefore useless
// against the exact failure mode worth interrupting (a turn wedged inside one
// tool call that never returns). The session, the sessionId, the transcript
// JSONL, the workspace and the full conversation context all survive, and the
// next turn runs normally in the same session with all of that history intact.
//
// That is what makes the operator's stall escalation possible at all: its
// handoff turn goes through POST /v1/messages, which 409s for as long as a turn
// holds the slot, so before this endpoint existed a stalled pod could only ever
// be stopped with a synthetic note in place of the agent's own.
//
// The write takes ptyMu so the single byte cannot land inside another
// sequence's paste block or its SubmitDelay window.
func (mgr *Manager) Interrupt() (string, error) {
	mgr.mu.Lock()
	state, stopping, w := mgr.state, mgr.stopping, mgr.w
	id := mgr.current
	mgr.mu.Unlock()
	if w == nil || stopping || state == Dead || state == Booting {
		mgr.m.InterruptsTotal.WithLabelValues("unavailable").Inc()
		return "", fmt.Errorf("%w (state=%s)", ErrInterruptUnavailable, state)
	}

	mgr.ptyMu.Lock()
	_, err := w.Write([]byte(InterruptSeq))
	mgr.ptyMu.Unlock()
	if err != nil {
		mgr.m.InterruptsTotal.WithLabelValues("write_fail").Inc()
		mgr.log.Error("interrupt write failed", "action", "turn_interrupt_write_fail",
			"turn_id", id, "err", err)
		return "", fmt.Errorf("write pty interrupt: %w", err)
	}
	mgr.m.InterruptsTotal.WithLabelValues("ok").Inc()
	mgr.log.Warn("interrupt written to pty", "action", "turn_interrupt", "turn_id", id)

	if id != "" {
		mgr.wg.Add(1)
		go mgr.awaitInterruptedTurn(id)
	}
	return id, nil
}

// awaitInterruptedTurn waits for the interruption to show up in the transcript
// and then resolves the turn.
//
// It exists because an ESC'd turn is terminal but produces NO terminal signal
// the wrapper already listens for: no `end_turn`, no stop_reason, and no Stop
// hook (verified in the spike transcript - the next stop_hook_summary entry
// belongs to the FOLLOWING turn). Every completion path in the manager keys on
// one of those, so without this loop the interrupt succeeds, the agent stops,
// and mgr.current is never cleared: the session sits Busy forever, POST
// /v1/messages 409s forever, and the interrupt has made the pod strictly less
// useful than leaving the turn alone would have.
//
// Polling rather than a Tailer hook because the wrapper must resolve the turn
// even with CCW_LOG_TRANSCRIPT=false, when no Tailer exists at all.
func (mgr *Manager) awaitInterruptedTurn(id string) {
	defer mgr.wg.Done()
	// Wall clock, not mgr.now: the injected clock is frozen in tests and a
	// deadline computed from it would never expire.
	deadline := time.Now().Add(interruptSettleTimeout)
	for {
		mgr.mu.Lock()
		current, path, stopping := mgr.current, mgr.transcriptPath, mgr.stopping
		mgr.mu.Unlock()
		if stopping || current != id {
			// Either the pod is going down or another terminal path (Complete,
			// failTurn, crash recovery) already resolved the turn. Nothing to do.
			return
		}
		if path != "" && transcript.WasInterrupted(path) {
			mgr.completeAfterInterrupt(id, path)
			return
		}
		if !time.Now().Before(deadline) {
			mgr.m.InterruptsTotal.WithLabelValues("unconfirmed").Inc()
			mgr.log.Error("interrupt: no interruption marker in the transcript before the deadline; turn left in flight",
				"action", "turn_interrupt_unconfirmed", "turn_id", id, "path", path,
				"timeout_ms", interruptSettleTimeout.Milliseconds())
			return
		}
		time.Sleep(interruptPollInterval)
	}
}

// completeAfterInterrupt resolves an ESC'd turn from the transcript, mirroring
// completeFromTranscript's finalize path (store write, terminal metric, token
// metering, fireDone) so the operator gets a real callback rather than silence.
//
// The result is turn.Failed with StopReason "interrupted" and whatever text the
// agent had produced by the time the ESC landed. Failed rather than Complete
// because the agent did not finish: reporting success here would let the
// operator advance a Task on a turn it deliberately cut short. The partial text
// is kept because it is often the only record of what the agent was doing, and
// it is the same text a human would have read on screen.
//
// It leaves the session Ready, which is the entire point - the next turn (the
// operator's handoff prompt) must be submittable immediately.
func (mgr *Manager) completeAfterInterrupt(id, path string) {
	finalText, usage, _, err := transcript.LastAssistant(path)
	if err != nil {
		// Not fatal: the turn must still resolve, or the interrupt has wedged
		// the session, which is the one outcome this whole path exists to
		// prevent. Resolve with no text.
		mgr.log.Warn("interrupt: transcript read failed; resolving the turn without partial text",
			"action", "turn_interrupt_complete", "turn_id", id, "path", path, "err", err)
	}
	var tokens []TurnTokens
	if tt, terr := transcript.SumTurnTokens(path); terr == nil {
		tokens = tt
	} else {
		mgr.log.Warn("interrupt: token sum failed; resolving without token metrics",
			"action", "turn_interrupt_complete", "turn_id", id, "err", terr)
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
	started := mgr.currentStarted
	if serr := mgr.store.Interrupt(id, finalText, usage, now); serr != nil {
		// Correlation bug: the record is missing. Do not bump metrics or fire a
		// phantom callback (matches Complete/failTurn, finding 6).
		mgr.mu.Unlock()
		mgr.log.Error("store.Interrupt returned error; skipping metrics and callback",
			"action", "turn_interrupt_complete", "turn_id", id, "err", serr)
		return
	}
	mgr.clearCurrentLocked(Ready)
	mgr.currentSessionID = ""
	mgr.m.TurnsTotal.WithLabelValues("interrupted").Inc()
	mgr.m.TurnDuration.Observe(now.Sub(started).Seconds())
	rec, _ := mgr.store.Get(id)
	mgr.mu.Unlock()

	mgr.meterTokens(HookResult{Usage: usage, TurnTokens: tokens})
	mgr.log.Warn("turn resolved after interrupt", "action", "turn_interrupt_complete",
		"turn_id", id, "final_text_len", len(finalText),
		"duration_ms", now.Sub(started).Milliseconds())
	mgr.fireDone(rec)
}
