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

// escRecordingPTY tags each write as the paste blob ("open"), the ESC byte
// ("esc"), or the bare submit keystroke ("close").
type escRecordingPTY struct {
	mu    sync.Mutex
	kinds []string
}

func (r *escRecordingPTY) Write(p []byte) (int, error) {
	kind := "close"
	switch {
	case strings.Contains(string(p), DefaultSubmitSeq.PasteStart):
		kind = "open"
	case string(p) == InterruptSeq:
		kind = "esc"
	}
	r.mu.Lock()
	r.kinds = append(r.kinds, kind)
	r.mu.Unlock()
	return len(p), nil
}

func (r *escRecordingPTY) Close() error { return nil }

func (r *escRecordingPTY) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.kinds...)
}

// TestPtyMu_InterruptNeverLandsInsideASubmitSequence drives Interrupt
// concurrently with a Submit whose SubmitDelay is long enough to guarantee a
// collision without the lock, and asserts the ESC never lands between a
// sequence's paste blob and its submit keystroke.
//
// An ESC dropped into that window is not a cancel: it arrives while the CLI is
// still assembling the bracketed paste, so it cancels nothing and corrupts the
// message being pasted. The turn then neither runs the intended prompt nor
// stops - the worst of both, and invisible from outside the pod.
func TestPtyMu_InterruptNeverLandsInsideASubmitSequence(t *testing.T) {
	rec := &escRecordingPTY{}
	store := turn.NewStore()
	now := time.Unix(100, 0)

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
		// Land inside the SubmitDelay window if ptyMu does not stop us.
		time.Sleep(40 * time.Millisecond)
		_, _ = mgr.Interrupt()
	}()
	wg.Wait()

	kinds := rec.snapshot()
	if len(kinds) != 3 {
		t.Fatalf("got %d pty writes, want 3 (open, close, esc in some order): %v", len(kinds), kinds)
	}
	for i, k := range kinds {
		if k == "open" {
			if i+1 >= len(kinds) || kinds[i+1] != "close" {
				t.Fatalf("ESC landed inside the paste window: %v", kinds)
			}
		}
	}
}
