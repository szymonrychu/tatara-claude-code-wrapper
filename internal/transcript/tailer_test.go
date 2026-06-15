package transcript

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureHandler is a slog.Handler that records all log entries.
type captureHandler struct {
	mu      sync.Mutex
	records []map[string]any
	buf     *bytes.Buffer
	inner   slog.Handler
}

func newCaptureHandler() *captureHandler {
	buf := &bytes.Buffer{}
	return &captureHandler{
		buf:   buf,
		inner: slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}),
	}
}

func (h *captureHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return true
}

func (h *captureHandler) Handle(ctx context.Context, r slog.Record) error {
	err := h.inner.Handle(ctx, r)
	if err != nil {
		return err
	}
	// Parse the last JSON line written to buf
	h.mu.Lock()
	defer h.mu.Unlock()
	data := h.buf.Bytes()
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	last := lines[len(lines)-1]
	var m map[string]any
	if jerr := json.Unmarshal(last, &m); jerr == nil {
		h.records = append(h.records, m)
	}
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &captureHandler{buf: h.buf, inner: h.inner.WithAttrs(attrs), records: h.records}
}

func (h *captureHandler) WithGroup(name string) slog.Handler {
	return &captureHandler{buf: h.buf, inner: h.inner.WithGroup(name), records: h.records}
}

// TestTailer_UnknownNonMessageType_ClampedInMetric verifies that a transcript
// entry with an unknown type (not in the fixed enum) passes the raw type to
// the log (cardinality-free) but maps to "other" in the metric counter.
func TestTailer_UnknownNonMessageType_ClampedInMetric(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	// Write a non-message entry with a future/unknown type
	writeTranscriptLine(t, path, `{"type":"future_unknown","uuid":"u1","sessionId":"s1","timestamp":"2026-06-15T00:00:00.000Z"}`)

	h := newCaptureHandler()
	log := slog.New(h)
	// Check the log record - stream_type in the log should be the raw type, and
	// we separately unit-test clampNonMessageType for the metric label.
	tailer := NewTailer(log, NewRedactor(nil), func() string { return "" })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- tailer.Follow(ctx, path) }()

	recs := waitForRecords(t, h, 1, 2*time.Second)
	cancel()
	<-done

	stream := filterAgentStream(recs)
	if len(stream) == 0 {
		t.Fatal("expected at least one agent_stream event")
	}
	// The log stream_type must keep the raw type (so the log is queryable).
	got := stream[0]
	if got["stream_type"] != "future_unknown" {
		t.Errorf("log stream_type = %v, want future_unknown (raw type preserved in log)", got["stream_type"])
	}
}

func TestClampNonMessageType(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"system", "system"},
		{"summary", "summary"},
		{"user", "user"},
		{"assistant", "assistant"},
		{"unknown_future_type", "other"},
		{"model_generated_label", "other"},
		{"", "other"},
	} {
		got := clampNonMessageType(tt.in)
		if got != tt.want {
			t.Errorf("clampNonMessageType(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func (h *captureHandler) Records() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]map[string]any, len(h.records))
	copy(out, h.records)
	return out
}

// readTestdata reads a file from ../session/testdata/ relative to this package.
func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "session", "testdata", name))
	if err != nil {
		t.Fatalf("readTestdata %s: %v", name, err)
	}
	return b
}

// waitForRecords polls until at least n records exist in handler or times out.
func waitForRecords(t *testing.T, h *captureHandler, n int, timeout time.Duration) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		recs := h.Records()
		if len(recs) >= n {
			return recs
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d log records, got %d", n, len(h.Records()))
	return nil
}

// filterAgentStream returns only agent_stream records.
func filterAgentStream(recs []map[string]any) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if r["action"] == "agent_stream" {
			out = append(out, r)
		}
	}
	return out
}

// writeTranscriptLine writes a JSONL line to a file (appends).
func writeTranscriptLine(t *testing.T, path string, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open transcript for write: %v", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatalf("write transcript line: %v", err)
	}
}

// assistantTextLine returns the testdata assistant line (real transcript from claude-code).
func assistantTextLine(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(string(readTestdata(t, "transcript_assistant_line.jsonl")))
}

// makeToolUseLine crafts a minimal tool_use content block line.
func makeToolUseLine() string {
	return `{"type":"assistant","uuid":"uuid-tool-1","sessionId":"sess-1","timestamp":"2026-06-11T10:00:00.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"command":"ls"}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}}`
}

// makeToolResultLine crafts a minimal tool_result content block line.
func makeToolResultLine() string {
	return `{"type":"user","uuid":"uuid-result-1","sessionId":"sess-1","timestamp":"2026-06-11T10:00:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"some output","is_error":false}]}}`
}

// makeThinkingLine crafts a minimal thinking content block line.
func makeThinkingLine() string {
	return `{"type":"assistant","uuid":"uuid-think-1","sessionId":"sess-1","timestamp":"2026-06-11T10:00:02.000Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"I should do this carefully"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":20}}}`
}

func TestTailer_TextEventFromRealTranscriptLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

	line := assistantTextLine(t)
	writeTranscriptLine(t, path, line)

	h := newCaptureHandler()
	log := slog.New(h)
	tailer := NewTailer(log, NewRedactor(nil), func() string { return "turn-1" })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- tailer.Follow(ctx, path) }()

	recs := waitForRecords(t, h, 1, 2*time.Second)
	cancel()
	<-done

	stream := filterAgentStream(recs)
	if len(stream) == 0 {
		t.Fatalf("no agent_stream records emitted")
	}
	got := stream[0]
	if got["stream_type"] != "text" {
		t.Errorf("stream_type=%v, want text", got["stream_type"])
	}
	if got["text"] != "PING" {
		t.Errorf("text=%v, want PING", got["text"])
	}
	if got["role"] != "assistant" {
		t.Errorf("role=%v, want assistant", got["role"])
	}
	if got["session_id"] == nil || got["session_id"] == "" {
		t.Errorf("expected session_id, got %v", got["session_id"])
	}
	if got["transcript_uuid"] == nil || got["transcript_uuid"] == "" {
		t.Errorf("expected transcript_uuid, got %v", got["transcript_uuid"])
	}
	if got["turn_id"] != "turn-1" {
		t.Errorf("turn_id=%v, want turn-1", got["turn_id"])
	}
}

func TestTailer_ToolUseEvent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	writeTranscriptLine(t, path, makeToolUseLine())

	h := newCaptureHandler()
	log := slog.New(h)
	tailer := NewTailer(log, NewRedactor(nil), func() string { return "" })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- tailer.Follow(ctx, path) }()

	recs := waitForRecords(t, h, 1, 2*time.Second)
	cancel()
	<-done

	stream := filterAgentStream(recs)
	if len(stream) == 0 {
		t.Fatalf("no agent_stream records")
	}
	// first record should be tool_use, second is message_end
	var toolUse map[string]any
	for _, r := range stream {
		if r["stream_type"] == "tool_use" {
			toolUse = r
			break
		}
	}
	if toolUse == nil {
		t.Fatalf("no tool_use stream event, got: %v", stream)
	}
	if toolUse["tool"] != "Bash" {
		t.Errorf("tool=%v, want Bash", toolUse["tool"])
	}
	if toolUse["tool_use_id"] != "toolu_01" {
		t.Errorf("tool_use_id=%v, want toolu_01", toolUse["tool_use_id"])
	}
	if toolUse["input"] == nil {
		t.Errorf("expected input field")
	}
}

func TestTailer_ToolResultEvent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	writeTranscriptLine(t, path, makeToolResultLine())

	h := newCaptureHandler()
	log := slog.New(h)
	tailer := NewTailer(log, NewRedactor(nil), func() string { return "" })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- tailer.Follow(ctx, path) }()

	recs := waitForRecords(t, h, 1, 2*time.Second)
	cancel()
	<-done

	stream := filterAgentStream(recs)
	var toolResult map[string]any
	for _, r := range stream {
		if r["stream_type"] == "tool_result" {
			toolResult = r
			break
		}
	}
	if toolResult == nil {
		t.Fatalf("no tool_result stream event, got: %v", stream)
	}
	if toolResult["tool_use_id"] != "toolu_01" {
		t.Errorf("tool_use_id=%v, want toolu_01", toolResult["tool_use_id"])
	}
}

func TestTailer_ThinkingEvent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	writeTranscriptLine(t, path, makeThinkingLine())

	h := newCaptureHandler()
	log := slog.New(h)
	tailer := NewTailer(log, NewRedactor(nil), func() string { return "" })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- tailer.Follow(ctx, path) }()

	recs := waitForRecords(t, h, 1, 2*time.Second)
	cancel()
	<-done

	stream := filterAgentStream(recs)
	var thinking map[string]any
	for _, r := range stream {
		if r["stream_type"] == "thinking" {
			thinking = r
			break
		}
	}
	if thinking == nil {
		t.Fatalf("no thinking stream event, got: %v", stream)
	}
	if thinking["text"] != "I should do this carefully" {
		t.Errorf("text=%v, want thinking text", thinking["text"])
	}
}

func TestTailer_MalformedLineEmitsRawEvent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	writeTranscriptLine(t, path, "this is not valid json {{{")

	h := newCaptureHandler()
	log := slog.New(h)
	tailer := NewTailer(log, NewRedactor(nil), func() string { return "" })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- tailer.Follow(ctx, path) }()

	recs := waitForRecords(t, h, 1, 2*time.Second)
	cancel()
	<-done

	stream := filterAgentStream(recs)
	var raw map[string]any
	for _, r := range stream {
		if r["stream_type"] == "raw" {
			raw = r
			break
		}
	}
	if raw == nil {
		t.Fatalf("expected raw event for malformed line, got: %v", stream)
	}
	if raw["raw_line"] == nil {
		t.Errorf("expected raw_line field")
	}
}

func TestTailer_AppendAfterOpenIsEmitted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

	// Start with existing content
	writeTranscriptLine(t, path, assistantTextLine(t))

	h := newCaptureHandler()
	log := slog.New(h)
	tailer := NewTailer(log, NewRedactor(nil), func() string { return "" })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- tailer.Follow(ctx, path) }()

	// Wait for first line to be consumed
	waitForRecords(t, h, 1, 2*time.Second)

	// Append a new line after tailer is already following
	writeTranscriptLine(t, path, makeToolUseLine())

	// Wait for the appended line to be processed (need at least 2 agent_stream events)
	// The tool_use line emits tool_use + message_end = 2 events; first line emits text + message_end = 2
	// So total >= 4 agent_stream events
	waitForRecords(t, h, 4, 3*time.Second)
	cancel()
	<-done

	stream := filterAgentStream(h.Records())
	var hasToolUse bool
	for _, r := range stream {
		if r["stream_type"] == "tool_use" {
			hasToolUse = true
			break
		}
	}
	if !hasToolUse {
		t.Fatalf("appended tool_use line was not emitted, got: %v", stream)
	}
}

func TestTailer_InodeChangeReopens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

	// Write initial content
	writeTranscriptLine(t, path, assistantTextLine(t))

	h := newCaptureHandler()
	log := slog.New(h)
	tailer := NewTailer(log, NewRedactor(nil), func() string { return "" })

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- tailer.Follow(ctx, path) }()

	// Wait for initial line
	waitForRecords(t, h, 1, 2*time.Second)

	// Simulate inode change: remove and recreate the file (like claude restart)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// New file with different content
	writeTranscriptLine(t, path, makeThinkingLine())

	// Should detect inode change and emit thinking event
	recs := waitForRecords(t, h, 3, 4*time.Second)
	cancel()
	<-done

	stream := filterAgentStream(recs)
	var hasThinking bool
	for _, r := range stream {
		if r["stream_type"] == "thinking" {
			hasThinking = true
			break
		}
	}
	if !hasThinking {
		t.Fatalf("inode change was not detected - thinking line not emitted, got: %v", stream)
	}
}

func TestTailer_MessageEndEvent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	writeTranscriptLine(t, path, assistantTextLine(t))

	h := newCaptureHandler()
	log := slog.New(h)
	tailer := NewTailer(log, NewRedactor(nil), func() string { return "" })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- tailer.Follow(ctx, path) }()

	// assistant line has text content + stop_reason, expect text + message_end
	recs := waitForRecords(t, h, 2, 2*time.Second)
	cancel()
	<-done

	stream := filterAgentStream(recs)
	var msgEnd map[string]any
	for _, r := range stream {
		if r["stream_type"] == "message_end" {
			msgEnd = r
			break
		}
	}
	if msgEnd == nil {
		t.Fatalf("expected message_end event, got: %v", stream)
	}
	if msgEnd["stop_reason"] == nil {
		t.Errorf("expected stop_reason in message_end")
	}
	if msgEnd["role"] != "assistant" {
		t.Errorf("role=%v, want assistant", msgEnd["role"])
	}
}

func TestTailer_RedactorAppliedToText(t *testing.T) {
	// Craft a line where the text field contains a secret
	secretVal := "supersecrettoken9999"
	line := `{"type":"assistant","uuid":"uuid-redact","sessionId":"sess-redact","timestamp":"2026-06-11T10:00:00.000Z","message":{"role":"assistant","content":[{"type":"text","text":"token is supersecrettoken9999 in text"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":5}}}`

	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	writeTranscriptLine(t, path, line)

	h := newCaptureHandler()
	log := slog.New(h)
	tailer := NewTailer(log, NewRedactor(map[string]string{"MY_SECRET": secretVal}), func() string { return "" })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- tailer.Follow(ctx, path) }()

	recs := waitForRecords(t, h, 1, 2*time.Second)
	cancel()
	<-done

	stream := filterAgentStream(recs)
	for _, r := range stream {
		if r["stream_type"] == "text" {
			text, _ := r["text"].(string)
			if strings.Contains(text, secretVal) {
				t.Errorf("secret value leaked in text field: %q", text)
			}
			if !strings.Contains(text, "[REDACTED:MY_SECRET]") {
				t.Errorf("expected redacted placeholder in text, got: %q", text)
			}
		}
	}
}
