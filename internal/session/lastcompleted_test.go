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
