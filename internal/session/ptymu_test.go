package session

import (
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

// recordingPTY logs each Write call as a timestamped event, tagged "open" if
// it carries the bracketed-paste start marker and "close" otherwise (the bare
// submit keystroke). It never errors, so it never triggers the manager's
// failure paths.
type recordingPTY struct {
	mu     sync.Mutex
	events []ptyEvent
}

type ptyEvent struct {
	kind string // "open" or "close"
	body string
}

func (r *recordingPTY) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kind := "close"
	if strings.Contains(string(p), DefaultSubmitSeq.PasteStart) {
		kind = "open"
	}
	r.events = append(r.events, ptyEvent{kind: kind, body: string(p)})
	return len(p), nil
}

func (r *recordingPTY) Close() error { return nil }

func (r *recordingPTY) snapshot() []ptyEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ptyEvent(nil), r.events...)
}

// TestPtyMu_SubmitAndResubmitDoNotInterleave drives Submit and
// resubmitOriginalPrompt concurrently against a recording PTY and asserts
// that one sequence's paste-start blob ("open") is always immediately
// followed by its own submit keystroke ("close") before the other sequence's
// blob can start. Without ptyMu serialising the two, the second sequence's
// blob write races in and lands between the first sequence's paste and its
// submit keystroke (open, open, close, close) instead of the required
// (open, close, open, close).
//
// This reproduces the corruption window phase W2 closes: today the turn slot
// (mgr.current) is the only thing serialising two paste+SubmitDelay+submit
// sequences, and resubmitOriginalPrompt (the crash-resume path) does not
// touch mgr.current at all, so nothing stops it from writing concurrently
// with an in-flight Submit.
func TestPtyMu_SubmitAndResubmitDoNotInterleave(t *testing.T) {
	rec := &recordingPTY{}
	store := turn.NewStore()
	now := time.Unix(100, 0)
	// Seed the record resubmitOriginalPrompt reads its prompt text from.
	store.Create("resume-1", "resumed-turn-payload", "", now)

	mgr := New(
		Config{
			TurnTimeout: 10 * time.Second,
			SubmitDelay: 150 * time.Millisecond,
			SubmitSeq:   DefaultSubmitSeq,
		},
		store, metrics.New(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() time.Time { return now },
		func() string { return "submit-1" },
	)
	mgr.SetWriterForTest(rec)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = mgr.Submit("submitted-turn-payload", "", false)
	}()
	go func() {
		defer wg.Done()
		// Exercises the crash-resume resubmit path directly: it writes the
		// same paste+SubmitDelay+submit sequence as Submit but is reached
		// without ever taking the turn slot, so it is exactly the kind of
		// second writer W3's mid-turn probe will introduce.
		mgr.resubmitOriginalPrompt("resume-1", rec, DefaultSubmitSeq, now)
	}()
	wg.Wait()

	events := rec.snapshot()
	if len(events) != 4 {
		t.Fatalf("got %d pty writes, want 4 (open,close,open,close): %+v", len(events), events)
	}
	for i, want := range []string{"open", "close", "open", "close"} {
		if events[i].kind != want {
			t.Fatalf("write interleaving detected: event %d = %q, want %q; full sequence: %+v",
				i, events[i].kind, want, events)
		}
	}
}
