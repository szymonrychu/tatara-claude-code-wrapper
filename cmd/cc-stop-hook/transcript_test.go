package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLastAssistantText_Synthetic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.jsonl")
	lines := `{"type":"user","message":{"content":[{"type":"text","text":"hi"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"first"}],"usage":{"output_tokens":1},"stop_reason":"tool_use"}}
{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"final answer"}],"usage":{"output_tokens":2},"stop_reason":"end_turn"}}
`
	require.NoError(t, os.WriteFile(p, []byte(lines), 0o644))
	text, usage, stop, err := lastAssistantText(p)
	require.NoError(t, err)
	require.Equal(t, "final answer", text)
	require.JSONEq(t, `{"output_tokens":2}`, string(usage))
	require.Equal(t, "end_turn", stop)
}

func TestLastAssistantText_StopReasonPropagatedToBuildResult(t *testing.T) {
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	lines := `{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}],"usage":{"output_tokens":5},"stop_reason":"end_turn"}}
`
	require.NoError(t, os.WriteFile(transcriptPath, []byte(lines), 0o644))

	payload := []byte(`{"session_id":"sess-1","transcript_path":"` + transcriptPath + `","last_assistant_message":"hello"}`)
	res, err := buildResult(payload, "/nonexistent/result.json")
	require.NoError(t, err)
	require.Equal(t, "end_turn", res.StopReason, "StopReason must be extracted from transcript")
}

// TestLastAssistantText_ToolOnlyFinalLine verifies that when the final assistant
// turn ends with a tool_use block (no text content) the stop_reason and usage
// from THAT final line are returned, not the values from an earlier text-bearing
// assistant line.
func TestLastAssistantText_ToolOnlyFinalLine(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.jsonl")
	// First line: text-bearing, earlier stop_reason "end_turn" and lower usage.
	// Second line: tool-only (no text), the authoritative final turn with "tool_use".
	lines := `{"type":"assistant","message":{"content":[{"type":"text","text":"let me check"}],"usage":{"output_tokens":5},"stop_reason":"end_turn"}}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}],"usage":{"output_tokens":10},"stop_reason":"tool_use"}}
`
	require.NoError(t, os.WriteFile(p, []byte(lines), 0o644))
	text, usage, stop, err := lastAssistantText(p)
	require.NoError(t, err)
	// lastText from the first (text-bearing) line is fine
	require.Equal(t, "let me check", text)
	// usage and stop_reason must come from the FINAL (tool-only) assistant line
	require.Equal(t, "tool_use", stop, "stop_reason must be from the final assistant line, not from an earlier text-bearing one")
	require.JSONEq(t, `{"output_tokens":10}`, string(usage), "usage must be from the final assistant line")
}

func TestLastAssistantText_FromRealTranscript(t *testing.T) {
	path := filepath.Join("testdata", "transcript.jsonl")
	if _, err := os.Stat(path); err != nil {
		t.Skip("spike fixture missing; run Task 1")
	}
	text, usage, stop, err := lastAssistantText(path)
	require.NoError(t, err)
	require.NotEmpty(t, text)
	require.Equal(t, "end_turn", stop)
	_ = usage // usage may be present; type is json.RawMessage
}
