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

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
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

// TestTailer_ThinkingFallbackRemovedEmitsRaw verifies that a thinking block
// with an empty Thinking field (and a non-empty Text field) is emitted as a
// raw event rather than silently falling back to the Text field. The fallback
// is dead code for a shape that never occurs and masks unexpected content.
func TestTailer_ThinkingFallbackRemovedEmitsRaw(t *testing.T) {
	// Craft a thinking block where thinking="" and text="some text" (invalid shape).
	line := `{"type":"assistant","uuid":"uuid-think-bad","sessionId":"sess-1","timestamp":"2026-06-11T10:00:02.000Z","message":{"role":"assistant","content":[{"type":"thinking","text":"fallback text","thinking":""}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	writeTranscriptLine(t, path, line)

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
	// Must NOT emit a thinking event with text="fallback text" (the dead fallback).
	// Must emit a raw event so the unexpected shape is visible.
	for _, r := range stream {
		if r["stream_type"] == "thinking" {
			t.Errorf("unexpected thinking event with text=%v; want raw event for empty-Thinking block", r["text"])
		}
	}
	var hasRaw bool
	for _, r := range stream {
		if r["stream_type"] == "raw" {
			hasRaw = true
			break
		}
	}
	if !hasRaw {
		t.Fatalf("expected raw event for empty-Thinking block, got: %v", stream)
	}
}

// TestTailer_TurnIDCapturedOnce verifies that processLine calls turnID() at
// most once, even for malformed and non-message paths. We do this by counting
// invocations; if it's called more than once for a single line that would be
// unnecessary mutex churn.
func TestTailer_TurnIDCapturedOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	// Write a malformed line AND a non-message line AND a message line.
	writeTranscriptLine(t, path, "not valid json {{{")
	writeTranscriptLine(t, path, `{"type":"system","uuid":"u2","sessionId":"s1","timestamp":"2026-06-15T00:00:00.000Z"}`)
	writeTranscriptLine(t, path, makeToolUseLine())

	var callCount int
	var mu sync.Mutex
	turnFn := func() string {
		mu.Lock()
		callCount++
		mu.Unlock()
		return "turn-x"
	}

	h := newCaptureHandler()
	log := slog.New(h)
	tailer := NewTailer(log, NewRedactor(nil), turnFn)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- tailer.Follow(ctx, path) }()

	// 3 lines: malformed(1 event) + system(1 event) + tool_use line (tool_use + message_end = 2 events)
	waitForRecords(t, h, 4, 2*time.Second)
	cancel()
	<-done

	mu.Lock()
	got := callCount
	mu.Unlock()

	// 3 lines processed -> at most 3 turnID calls (one per line).
	if got > 3 {
		t.Errorf("turnID called %d times for 3 lines, want <= 3 (once per line)", got)
	}
}

// TestTailer_FiresActivityPerLine verifies the OnActivity hook fires once per
// processed line carrying an in-flight turn id (the liveness heartbeat), even
// when a single line emits multiple content-block events.
func TestTailer_FiresActivityPerLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	writeTranscriptLine(t, path, makeToolUseLine())    // tool_use + message_end = 2 events
	writeTranscriptLine(t, path, makeToolResultLine()) // tool_result = 1 event

	var mu sync.Mutex
	var got []string
	h := newCaptureHandler()
	tailer := NewTailer(slog.New(h), NewRedactor(nil), func() string { return "turn-1" })
	tailer.WithActivity(func(id string) {
		mu.Lock()
		got = append(got, id)
		mu.Unlock()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tailer.Follow(ctx, path) }()

	waitForRecords(t, h, 3, 2*time.Second) // 2 lines -> 3 agent_stream events
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("OnActivity fired %d times, want 2 (once per line, not per content block): %v", len(got), got)
	}
	for _, id := range got {
		if id != "turn-1" {
			t.Errorf("OnActivity got turn id %q, want turn-1", id)
		}
	}
}

// TestTailer_NoActivityWhenNoInFlightTurn verifies the OnActivity hook is not
// fired for lines processed while no turn is in flight (turnID == "").
func TestTailer_NoActivityWhenNoInFlightTurn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	writeTranscriptLine(t, path, makeToolUseLine())

	var mu sync.Mutex
	var fired int
	h := newCaptureHandler()
	tailer := NewTailer(slog.New(h), NewRedactor(nil), func() string { return "" })
	tailer.WithActivity(func(string) {
		mu.Lock()
		fired++
		mu.Unlock()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tailer.Follow(ctx, path) }()

	waitForRecords(t, h, 2, 2*time.Second)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if fired != 0 {
		t.Fatalf("OnActivity fired %d times with no in-flight turn, want 0", fired)
	}
}

// TestTailer_PartialLineSizeCap verifies that a partial line that exceeds
// maxPartialBytes is flushed as a raw event and partial is reset, preventing
// unbounded memory growth from a never-newline-terminated chunk.
func TestTailer_PartialLineSizeCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")

	// Write a chunk that exceeds maxPartialBytes with no trailing newline.
	// We use a small sentinel constant here; the production constant is 16MiB
	// which is too large to allocate in a unit test. Instead we write content
	// directly and test the flush logic by calling processLine indirectly
	// through the Follow loop with a file that has a very long partial segment.
	//
	// Strategy: write maxPartialBytes+1 bytes of 'x' then a newline.  The file
	// has exactly one "line" that is maxPartialBytes+1 bytes long (so it is read
	// past the cap).  We verify that a raw event is emitted.
	oversized := make([]byte, maxPartialBytes+1)
	for i := range oversized {
		oversized[i] = 'x'
	}
	oversized = append(oversized, '\n')
	if err := os.WriteFile(path, oversized, 0o600); err != nil {
		t.Fatalf("write oversized file: %v", err)
	}

	h := newCaptureHandler()
	log := slog.New(h)
	tailer := NewTailer(log, NewRedactor(nil), func() string { return "" })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- tailer.Follow(ctx, path) }()

	recs := waitForRecords(t, h, 1, 3*time.Second)
	cancel()
	<-done

	stream := filterAgentStream(recs)
	var hasRaw bool
	for _, r := range stream {
		if r["stream_type"] == "raw" {
			hasRaw = true
			break
		}
	}
	if !hasRaw {
		t.Fatalf("expected raw event for oversized partial line, got: %v", stream)
	}
}

// TestTailer_UsageFieldScrubbedInMessageEnd verifies that the usage field in a
// message_end event is run through the redactor, consistent with every other
// content field. A secret embedded in a model-supplied usage JSON blob must not
// leak into logs.
func TestTailer_UsageFieldScrubbedInMessageEnd(t *testing.T) {
	secretVal := "secrettoken8888"
	// Craft an assistant line whose usage JSON embeds the secret.
	line := `{"type":"assistant","uuid":"uuid-usage-redact","sessionId":"sess-usage","timestamp":"2026-06-11T10:00:00.000Z","message":{"role":"assistant","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1,"note":"secrettoken8888"}}}`

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

	// text + message_end
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
	usage, _ := msgEnd["usage"].(string)
	if strings.Contains(usage, secretVal) {
		t.Errorf("secret leaked in message_end usage field: %q", usage)
	}
}

// TestTailer_ReadErrorRetriesRatherThanExiting verifies that a non-EOF read
// error (simulated via a bad reader injected via a custom path) does NOT
// permanently kill the Follow goroutine. After the error the tailer should
// reopen and continue emitting events.
//
// Implementation note: we cannot inject a failing reader directly into Follow
// without refactoring the internal openFile closure, so we test the observable
// behaviour: write a valid line, replace the file while the tailer is running
// (triggering the inode-change reopen path), and confirm the tailer survives
// and emits the new content. The actual transient-error path (return err guard)
// is covered by the code-level fix; the integration is exercised by the
// inode-change test. Here we add a regression marker that the tailer does NOT
// return a non-context error under normal usage.
// TestTailer_ReadErrorDoesNotKillTailer verifies that a transient read error
// (simulated by replacing the file fd mid-tail via inode change after a bad
// chmod window) does NOT cause Follow to return early. Before the fix, `return
// err` on any non-EOF error would permanently end transcript streaming.
//
// Strategy: write one valid line, let tailer consume it, then remove+recreate
// the file. On the first inode-change poll the tailer calls os.Stat; if Stat
// succeeds the tailer reopens. We then write a second valid line and assert it
// is also consumed - proving the tailer survived any transient state.
func TestTailer_ReadErrorDoesNotKillTailer(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("chmod test unreliable as root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	writeTranscriptLine(t, path, assistantTextLine(t))

	h := newCaptureHandler()
	log := slog.New(h)
	tailer := NewTailer(log, NewRedactor(nil), func() string { return "" })

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- tailer.Follow(ctx, path) }()

	// Wait for initial line consumption (text + message_end = 2 events).
	waitForRecords(t, h, 2, 2*time.Second)

	// Make the file unreadable to trigger a read error, then restore and
	// replace with a new file (inode change) that has additional content.
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	time.Sleep(2 * pollInterval)
	if err := os.Remove(path); err != nil {
		// Restore perms first so Remove can work on some OSes
		_ = os.Chmod(path, 0o600)
		_ = os.Remove(path)
	}
	writeTranscriptLine(t, path, makeToolUseLine())

	// Expect tailer to recover and emit tool_use + message_end from the new file.
	recs := waitForRecords(t, h, 4, 4*time.Second)

	cancel()
	err := <-done
	if err != nil && err != context.Canceled && err != context.DeadlineExceeded {
		t.Errorf("Follow returned non-context error %v; want context error only", err)
	}

	stream := filterAgentStream(recs)
	var hasToolUse bool
	for _, r := range stream {
		if r["stream_type"] == "tool_use" {
			hasToolUse = true
			break
		}
	}
	if !hasToolUse {
		t.Fatalf("tailer did not recover after read error; tool_use event missing, got: %v", stream)
	}
}

// fakeInternalIssueCounter is a test double for InternalIssueCounter.
type fakeInternalIssueCounter struct {
	mu   sync.Mutex
	hits []struct{ category, severity string }
}

func (f *fakeInternalIssueCounter) WithLabelValues(lvs ...string) prometheus.Counter {
	f.mu.Lock()
	cat, sev := "", ""
	if len(lvs) > 0 {
		cat = lvs[0]
	}
	if len(lvs) > 1 {
		sev = lvs[1]
	}
	f.hits = append(f.hits, struct{ category, severity string }{cat, sev})
	f.mu.Unlock()
	return &noopCounter{}
}

func (f *fakeInternalIssueCounter) Calls() []struct{ category, severity string } {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]struct{ category, severity string }, len(f.hits))
	copy(out, f.hits)
	return out
}

// noopCounter implements prometheus.Counter with no-ops.
type noopCounter struct{}

func (n *noopCounter) Desc() *prometheus.Desc              { return nil }
func (n *noopCounter) Write(*dto.Metric) error             { return nil }
func (n *noopCounter) Describe(ch chan<- *prometheus.Desc) {}
func (n *noopCounter) Collect(ch chan<- prometheus.Metric) {}
func (n *noopCounter) Inc()                                {}
func (n *noopCounter) Add(float64)                         {}

// makeInternalIssueLine returns a transcript line for tool_use of
// report_internal_issue with the given JSON input string.
func makeInternalIssueLine(inputJSON string) string {
	return `{"type":"assistant","uuid":"uuid-ii-1","sessionId":"sess-ii","timestamp":"2026-06-23T10:00:00.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_ii","name":"mcp__tatara__report_internal_issue","input":` + inputJSON + `}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":2}}}`
}

// filterByAction returns records matching the given action field.
func filterByAction(recs []map[string]any, action string) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if r["action"] == action {
			out = append(out, r)
		}
	}
	return out
}

// TestInternalIssueToolNameConst asserts the match string is the exact namespaced
// tool name. If the cli server name or tool name changes, the wrapper silently
// stops emitting; this test catches the drift.
func TestInternalIssueToolNameConst(t *testing.T) {
	const want = "mcp__tatara__report_internal_issue"
	if internalIssueToolName != want {
		t.Errorf("internalIssueToolName = %q, want %q", internalIssueToolName, want)
	}
}

// TestTailer_InternalIssue_ValidInput verifies that a tool_use block named
// mcp__tatara__report_internal_issue causes:
// (a) the generic agent_stream INFO line still emits (stream_type=tool_use),
// (b) a distinct ERROR record with action=internal_issue_report + correct fields,
// (c) InternalIssueCounter incremented with clamped labels.
func TestTailer_InternalIssue_ValidInput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	input := `{"category":"tool_error","severity":"error","description":"the tool blew up","offending_tool":"Bash","resource_id":"res-1"}`
	writeTranscriptLine(t, path, makeInternalIssueLine(input))

	h := newCaptureHandler()
	fc := &fakeInternalIssueCounter{}
	tailer := NewTailer(slog.New(h), NewRedactor(nil), func() string { return "turn-ii" }).
		WithInternalIssueCounter(fc)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tailer.Follow(ctx, path) }()

	// agent_stream(tool_use) + internal_issue_report(ERROR) + message_end = 3 records
	recs := waitForRecords(t, h, 3, 2*time.Second)
	cancel()
	<-done

	// (a) generic agent_stream tool_use INFO still emits
	agentStream := filterAgentStream(recs)
	var hasToolUse bool
	for _, r := range agentStream {
		if r["stream_type"] == "tool_use" {
			hasToolUse = true
			if r["tool"] != "mcp__tatara__report_internal_issue" {
				t.Errorf("tool=%v, want mcp__tatara__report_internal_issue", r["tool"])
			}
		}
	}
	if !hasToolUse {
		t.Fatalf("no tool_use agent_stream record emitted, got: %v", agentStream)
	}

	// (b) distinct internal_issue_report ERROR record
	issueRecs := filterByAction(recs, "internal_issue_report")
	if len(issueRecs) == 0 {
		t.Fatalf("no internal_issue_report record emitted, got: %v", recs)
	}
	ir := issueRecs[0]
	if ir["level"] != "ERROR" {
		t.Errorf("level=%v, want ERROR", ir["level"])
	}
	if ir["category"] != "tool_error" {
		t.Errorf("category=%v, want tool_error", ir["category"])
	}
	if ir["severity"] != "error" {
		t.Errorf("severity=%v, want error", ir["severity"])
	}
	if ir["description"] != "the tool blew up" {
		t.Errorf("description=%v, want 'the tool blew up'", ir["description"])
	}
	if ir["offending_tool"] != "Bash" {
		t.Errorf("offending_tool=%v, want Bash", ir["offending_tool"])
	}
	if ir["resource_id"] != "res-1" {
		t.Errorf("resource_id=%v, want res-1", ir["resource_id"])
	}

	// (c) counter incremented with correct labels
	calls := fc.Calls()
	if len(calls) == 0 {
		t.Fatal("InternalIssueCounter not incremented")
	}
	if calls[0].category != "tool_error" || calls[0].severity != "error" {
		t.Errorf("counter labels = (%q, %q), want (tool_error, error)", calls[0].category, calls[0].severity)
	}
}

// TestTailer_InternalIssue_WarnSeverity verifies warn severity maps to Warn log level.
func TestTailer_InternalIssue_WarnSeverity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	input := `{"category":"workspace_broken","severity":"warn","description":"workspace smells wrong"}`
	writeTranscriptLine(t, path, makeInternalIssueLine(input))

	h := newCaptureHandler()
	fc := &fakeInternalIssueCounter{}
	tailer := NewTailer(slog.New(h), NewRedactor(nil), func() string { return "" }).
		WithInternalIssueCounter(fc)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tailer.Follow(ctx, path) }()
	// agent_stream(tool_use) + internal_issue_report(WARN) + message_end = 3 records
	recs := waitForRecords(t, h, 3, 2*time.Second)
	cancel()
	<-done

	issueRecs := filterByAction(recs, "internal_issue_report")
	if len(issueRecs) == 0 {
		t.Fatalf("no internal_issue_report record, got: %v", recs)
	}
	if issueRecs[0]["level"] != "WARN" {
		t.Errorf("level=%v, want WARN for severity=warn", issueRecs[0]["level"])
	}
	calls := fc.Calls()
	if len(calls) == 0 || calls[0].severity != "warn" {
		t.Errorf("counter severity=%v, want warn", calls)
	}
}

// TestTailer_InternalIssue_MissingSeverityDefaultsError verifies missing severity
// defaults to error.
func TestTailer_InternalIssue_MissingSeverityDefaultsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	input := `{"category":"auth","description":"auth broke"}`
	writeTranscriptLine(t, path, makeInternalIssueLine(input))

	h := newCaptureHandler()
	fc := &fakeInternalIssueCounter{}
	tailer := NewTailer(slog.New(h), NewRedactor(nil), func() string { return "" }).
		WithInternalIssueCounter(fc)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tailer.Follow(ctx, path) }()
	recs := waitForRecords(t, h, 3, 2*time.Second)
	cancel()
	<-done

	issueRecs := filterByAction(recs, "internal_issue_report")
	if len(issueRecs) == 0 {
		t.Fatalf("no internal_issue_report record, got: %v", recs)
	}
	if issueRecs[0]["level"] != "ERROR" {
		t.Errorf("level=%v, want ERROR for missing severity", issueRecs[0]["level"])
	}
	calls := fc.Calls()
	if len(calls) == 0 || calls[0].severity != "error" {
		t.Errorf("counter severity=%v, want error (default)", calls)
	}
}

// TestTailer_InternalIssue_UnknownCategoryClampedToOther verifies unknown
// category is clamped to "other" in the counter but raw value is logged.
func TestTailer_InternalIssue_UnknownCategoryClampedToOther(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	input := `{"category":"totally_unknown_future_thing","severity":"error","description":"weird thing"}`
	writeTranscriptLine(t, path, makeInternalIssueLine(input))

	h := newCaptureHandler()
	fc := &fakeInternalIssueCounter{}
	tailer := NewTailer(slog.New(h), NewRedactor(nil), func() string { return "" }).
		WithInternalIssueCounter(fc)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tailer.Follow(ctx, path) }()
	recs := waitForRecords(t, h, 3, 2*time.Second)
	cancel()
	<-done

	// Counter must use clamped label
	calls := fc.Calls()
	if len(calls) == 0 || calls[0].category != "other" {
		t.Errorf("counter category=%v, want other for unknown input", calls)
	}
	// Log carries raw unclamped value for queryability
	issueRecs := filterByAction(recs, "internal_issue_report")
	if len(issueRecs) == 0 {
		t.Fatalf("no internal_issue_report record, got: %v", recs)
	}
	if issueRecs[0]["category"] != "totally_unknown_future_thing" {
		t.Errorf("log category=%v, want raw unclamped value", issueRecs[0]["category"])
	}
}

// TestTailer_InternalIssue_MalformedJSONLogsErrorAndCounts verifies that a
// malformed input JSON causes an ERROR log with parse_error field and counter
// increment (category=other, severity=error) without panic.
func TestTailer_InternalIssue_MalformedJSONLogsErrorAndCounts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	// valid outer transcript but the tool_use Input field is invalid JSON embedded
	// as a raw string that the content block unmarshal captures as RawMessage, then
	// emitInternalIssue fails to unmarshal it further.
	input := `"not an object at all"`
	writeTranscriptLine(t, path, makeInternalIssueLine(input))

	h := newCaptureHandler()
	fc := &fakeInternalIssueCounter{}
	tailer := NewTailer(slog.New(h), NewRedactor(nil), func() string { return "" }).
		WithInternalIssueCounter(fc)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tailer.Follow(ctx, path) }()
	// agent_stream(tool_use) + internal_issue_report(ERROR parse_error) + message_end = 3 records
	recs := waitForRecords(t, h, 3, 2*time.Second)
	cancel()
	<-done

	// Counter must still be incremented (never drop the signal)
	calls := fc.Calls()
	if len(calls) == 0 {
		t.Fatal("InternalIssueCounter not incremented on malformed JSON")
	}
	if calls[0].category != "other" || calls[0].severity != "error" {
		t.Errorf("counter labels = (%q, %q), want (other, error) on parse failure", calls[0].category, calls[0].severity)
	}

	// Must emit an ERROR log with parse_error field
	issueRecs := filterByAction(recs, "internal_issue_report")
	if len(issueRecs) == 0 {
		t.Fatalf("no internal_issue_report record on parse failure, got: %v", recs)
	}
	ir := issueRecs[0]
	if ir["level"] != "ERROR" {
		t.Errorf("level=%v, want ERROR on parse failure", ir["level"])
	}
	if ir["parse_error"] == nil || ir["parse_error"] == "" {
		t.Errorf("expected parse_error field on parse failure, got: %v", ir)
	}
}

// TestTailer_InternalIssue_AllCategories verifies each valid category passes
// through without clamping.
func TestTailer_InternalIssue_AllCategories(t *testing.T) {
	categories := []string{
		"tool_error", "directive_contradiction", "workspace_broken",
		"memory_inconsistent", "graph_inconsistent", "auth", "other",
	}
	for _, cat := range categories {
		cat := cat
		t.Run(cat, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "transcript.jsonl")
			input := `{"category":"` + cat + `","severity":"error","description":"test ` + cat + `"}`
			writeTranscriptLine(t, path, makeInternalIssueLine(input))

			h := newCaptureHandler()
			fc := &fakeInternalIssueCounter{}
			tailer := NewTailer(slog.New(h), NewRedactor(nil), func() string { return "" }).
				WithInternalIssueCounter(fc)

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- tailer.Follow(ctx, path) }()
			recs := waitForRecords(t, h, 3, 2*time.Second)
			cancel()
			<-done

			calls := fc.Calls()
			if len(calls) == 0 {
				t.Fatalf("counter not incremented for category=%s", cat)
			}
			if calls[0].category != cat {
				t.Errorf("counter category=%q, want %q", calls[0].category, cat)
			}
			issueRecs := filterByAction(recs, "internal_issue_report")
			if len(issueRecs) == 0 {
				t.Fatalf("no internal_issue_report record for category=%s, got: %v", cat, recs)
			}
		})
	}
}

// TestTailer_InternalIssue_SecretFieldsRedacted verifies that secret values in
// the description, offending_tool, and resource_id fields of an
// internal_issue_report are scrubbed before logging. The generic agent_stream
// INFO record still emits (tool_use), the distinct internal_issue_report record
// must NOT contain the raw secret, and the counter must still be incremented.
func TestTailer_InternalIssue_SecretFieldsRedacted(t *testing.T) {
	fixtureVal := "ZZZ-FIXTURE-VALUE-0001"
	input := `{"category":"tool_error","severity":"error","description":"value is ZZZ-FIXTURE-VALUE-0001 leaked","offending_tool":"ZZZ-FIXTURE-VALUE-0001","resource_id":"res/ZZZ-FIXTURE-VALUE-0001"}`

	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	writeTranscriptLine(t, path, makeInternalIssueLine(input))

	h := newCaptureHandler()
	fc := &fakeInternalIssueCounter{}
	tailer := NewTailer(slog.New(h), NewRedactor(map[string]string{"MY_KEY": fixtureVal}), func() string { return "turn-redact" }).
		WithInternalIssueCounter(fc)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tailer.Follow(ctx, path) }()

	// agent_stream(tool_use) + internal_issue_report(ERROR) + message_end = 3 records
	recs := waitForRecords(t, h, 3, 2*time.Second)
	cancel()
	<-done

	// (a) generic agent_stream tool_use INFO still emits
	agentStream := filterAgentStream(recs)
	var hasToolUse bool
	for _, r := range agentStream {
		if r["stream_type"] == "tool_use" {
			hasToolUse = true
		}
	}
	if !hasToolUse {
		t.Fatalf("no tool_use agent_stream record emitted, got: %v", agentStream)
	}

	// (b) internal_issue_report record must not contain raw secret
	issueRecs := filterByAction(recs, "internal_issue_report")
	if len(issueRecs) == 0 {
		t.Fatalf("no internal_issue_report record emitted, got: %v", recs)
	}
	ir := issueRecs[0]

	desc, _ := ir["description"].(string)
	if strings.Contains(desc, fixtureVal) {
		t.Errorf("secret leaked in description: %q", desc)
	}
	if !strings.Contains(desc, "[REDACTED:MY_KEY]") {
		t.Errorf("description not redacted, got: %q", desc)
	}

	offTool, _ := ir["offending_tool"].(string)
	if strings.Contains(offTool, fixtureVal) {
		t.Errorf("secret leaked in offending_tool: %q", offTool)
	}
	if !strings.Contains(offTool, "[REDACTED:MY_KEY]") {
		t.Errorf("offending_tool not redacted, got: %q", offTool)
	}

	resID, _ := ir["resource_id"].(string)
	if strings.Contains(resID, fixtureVal) {
		t.Errorf("secret leaked in resource_id: %q", resID)
	}
	if !strings.Contains(resID, "[REDACTED:MY_KEY]") {
		t.Errorf("resource_id not redacted, got: %q", resID)
	}

	// (c) counter still incremented with correct labels
	calls := fc.Calls()
	if len(calls) == 0 {
		t.Fatal("InternalIssueCounter not incremented")
	}
	if calls[0].category != "tool_error" || calls[0].severity != "error" {
		t.Errorf("counter labels = (%q, %q), want (tool_error, error)", calls[0].category, calls[0].severity)
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
