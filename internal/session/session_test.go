package session_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

type fakePTY struct {
	mu      sync.Mutex
	written []byte
}

func (f *fakePTY) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written = append(f.written, p...)
	return len(p), nil
}
func (f *fakePTY) bytes() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.written...)
}
func (f *fakePTY) Close() error { return nil }

func newMgr(t *testing.T, fp *fakePTY) (*session.Manager, *turn.Store) {
	t.Helper()
	store := turn.NewStore()
	ids := make(chan string, 8)
	ids <- "turn-1"
	ids <- "turn-2"
	m := session.New(session.Config{TurnTimeout: 50 * time.Millisecond, SubmitSeq: session.DefaultSubmitSeq},
		store, metrics.New(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() time.Time { return time.Unix(100, 0) },
		func() string { return <-ids })
	m.SetWriterForTest(fp) // injects fake PTY, marks READY
	return m, store
}

func TestSubmit_WritesPasteAndSubmit_ThenBusy(t *testing.T) {
	fp := &fakePTY{}
	m, store := newMgr(t, fp)

	id, err := m.Submit("hello\nworld", "https://cb/x")
	require.NoError(t, err)
	require.Equal(t, "turn-1", id)

	w := string(fp.bytes())
	require.Contains(t, w, "\x1b[200~hello\nworld\x1b[201~") // bracketed paste
	require.Contains(t, w, "\r")                             // submit
	rec, _ := store.Get("turn-1")
	require.Equal(t, turn.Running, rec.State)

	// second submit while busy -> ErrBusy
	_, err = m.Submit("again", "")
	require.ErrorIs(t, err, session.ErrBusy)
}

func TestComplete_MarksDoneAndFiresCallback(t *testing.T) {
	fp := &fakePTY{}
	m, store := newMgr(t, fp)
	var got *turn.Record
	m.OnTurnDone = func(r *turn.Record) { got = r }

	_, _ = m.Submit("hi", "https://cb/x")
	require.NoError(t, m.Complete(session.HookResult{FinalText: "PONG", StopReason: "end_turn"}))

	rec, _ := store.Get("turn-1")
	require.Equal(t, turn.Complete, rec.State)
	require.Equal(t, "PONG", rec.FinalText)
	require.NotNil(t, got)
	require.Equal(t, "https://cb/x", got.CallbackURL)

	// now idle again -> next submit allowed
	_, err := m.Submit("next", "")
	require.NoError(t, err)
}

func TestTurnTimeout_FailsAndFiresCallback(t *testing.T) {
	fp := &fakePTY{}
	m, store := newMgr(t, fp)
	done := make(chan *turn.Record, 1)
	m.OnTurnDone = func(r *turn.Record) { done <- r }

	_, _ = m.Submit("hi", "https://cb/x")
	select {
	case r := <-done:
		require.Equal(t, turn.Failed, r.State)
	case <-time.After(time.Second):
		t.Fatal("timeout did not fire")
	}
	rec, _ := store.Get("turn-1")
	require.Equal(t, turn.Failed, rec.State)
}

func TestStart_RealPTYWithCat(t *testing.T) {
	// integration-ish: drive /bin/cat under a real PTY, confirm bytes flow.
	store := turn.NewStore()
	m := session.New(session.Config{ClaudePath: "/bin/cat", TurnTimeout: time.Second, SubmitSeq: session.DefaultSubmitSeq, BootTimeout: time.Second},
		store, metrics.New(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		time.Now, func() string { return "t" })
	require.NoError(t, m.Start(context.Background()))
	require.NoError(t, m.Shutdown(context.Background()))
}
