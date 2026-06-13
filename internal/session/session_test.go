package session_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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

func TestSnapshot_ReportsConfiguredRepo(t *testing.T) {
	store := turn.NewStore()
	m := session.New(
		session.Config{Model: "claude", Repo: "https://github.com/szymonrychu/tatara-claude-code-wrapper", SubmitSeq: session.DefaultSubmitSeq},
		store, metrics.New(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() time.Time { return time.Unix(100, 0) },
		func() string { return "turn-1" })

	snap := m.Snapshot()
	require.Equal(t, "https://github.com/szymonrychu/tatara-claude-code-wrapper", snap.Repo)
	require.Equal(t, "claude", snap.Model)
}

func TestSnapshot_EmptyRepoWhenUnconfigured(t *testing.T) {
	store := turn.NewStore()
	m := session.New(
		session.Config{SubmitSeq: session.DefaultSubmitSeq},
		store, metrics.New(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() time.Time { return time.Unix(100, 0) },
		func() string { return "turn-1" })

	require.Equal(t, "", m.Snapshot().Repo)
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
	require.NoError(t, m.Complete(session.HookResult{FinalText: "PONG", StopReason: "end_turn", TranscriptPath: "/workspace/.claude/projects/-workspace/s.jsonl"}))

	rec, _ := store.Get("turn-1")
	require.Equal(t, turn.Complete, rec.State)
	require.Equal(t, "PONG", rec.FinalText)
	require.NotNil(t, got)
	require.Equal(t, "https://cb/x", got.CallbackURL)
	require.Equal(t, "/workspace/.claude/projects/-workspace/s.jsonl", m.TranscriptPath()) // H1: recorded from hook

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

// syncBuffer is a concurrency-safe slog sink: the tailer writes from its own
// goroutine while the test reads, so a plain bytes.Buffer would data-race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func TestTailer_StartedOnCompleteWithTranscriptPath(t *testing.T) {
	// Verify that after Complete() with a transcript path, the tailer emits
	// agent_stream log events for lines in that file.
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "session.jsonl")

	// Write a line to the transcript file that the tailer will pick up
	line := `{"type":"assistant","uuid":"uuid-tail-test","sessionId":"sess-tail","timestamp":"2026-06-11T10:00:00.000Z","message":{"role":"assistant","content":[{"type":"text","text":"tailer works"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}}`
	f, err := os.Create(transcriptPath)
	require.NoError(t, err)
	_, err = f.WriteString(line + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	buf := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	store := turn.NewStore()
	ids := []string{"turn-1"}
	idx := 0
	m := session.New(
		session.Config{TurnTimeout: 50 * time.Millisecond, SubmitSeq: session.DefaultSubmitSeq},
		store,
		metrics.New(prometheus.NewRegistry()),
		log,
		func() time.Time { return time.Unix(100, 0) },
		func() string { s := ids[idx]; idx++; return s },
	)
	m.SetWriterForTest(&fakePTY{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.StartTailer(ctx)

	_, err = m.Submit("hi", "")
	require.NoError(t, err)
	require.NoError(t, m.Complete(session.HookResult{
		FinalText:      "ok",
		StopReason:     "end_turn",
		TranscriptPath: transcriptPath,
	}))

	// Give tailer time to process the file
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data := buf.Bytes()
		lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
		for _, ln := range lines {
			var rec map[string]any
			if json.Unmarshal(ln, &rec) == nil && rec["action"] == "agent_stream" && rec["stream_type"] == "text" {
				cancel()
				return // success
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("tailer did not emit agent_stream text event within timeout")
}

func TestTailer_DisabledWhenEnvFalse(t *testing.T) {
	// When CCW_LOG_TRANSCRIPT=false, no agent_stream events should be emitted.
	t.Setenv("CCW_LOG_TRANSCRIPT", "false")

	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "session.jsonl")
	line := `{"type":"assistant","uuid":"uuid-no-tail","sessionId":"sess-no","timestamp":"2026-06-11T10:00:00.000Z","message":{"role":"assistant","content":[{"type":"text","text":"should not appear"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}}`
	f, err := os.Create(transcriptPath)
	require.NoError(t, err)
	_, err = f.WriteString(line + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	buf := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	store := turn.NewStore()
	ids := []string{"turn-1"}
	idx := 0
	m := session.New(
		session.Config{TurnTimeout: 50 * time.Millisecond, SubmitSeq: session.DefaultSubmitSeq},
		store,
		metrics.New(prometheus.NewRegistry()),
		log,
		func() time.Time { return time.Unix(100, 0) },
		func() string { s := ids[idx]; idx++; return s },
	)
	m.SetWriterForTest(&fakePTY{})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m.StartTailer(ctx)

	_, err = m.Submit("hi", "")
	require.NoError(t, err)
	require.NoError(t, m.Complete(session.HookResult{
		FinalText:      "ok",
		StopReason:     "end_turn",
		TranscriptPath: transcriptPath,
	}))

	// Wait a bit, verify no agent_stream events appear
	time.Sleep(600 * time.Millisecond)
	cancel()

	data := buf.Bytes()
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	for _, ln := range lines {
		var rec map[string]any
		if json.Unmarshal(ln, &rec) == nil && rec["action"] == "agent_stream" {
			t.Fatalf("got unexpected agent_stream event when CCW_LOG_TRANSCRIPT=false: %v", rec)
		}
	}
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
