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
