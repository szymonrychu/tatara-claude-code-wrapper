package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
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

// TestTurnTokens_SumsCurrentTurnOnly verifies that token usage is summed across
// every assistant line of the LAST turn, that a typed user prompt resets the
// accumulator (prior turns are excluded), and that a tool_result user line
// (array content) does NOT reset it.
func TestTurnTokens_SumsCurrentTurnOnly(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.jsonl")
	lines := `{"type":"user","message":{"role":"user","content":"first prompt"}}
{"type":"assistant","message":{"model":"claude-opus-4-8","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":999,"output_tokens":999,"cache_read_input_tokens":999,"cache_creation_input_tokens":999}}}
{"type":"user","message":{"role":"user","content":"second prompt"}}
{"type":"assistant","message":{"model":"claude-opus-4-8","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}],"usage":{"input_tokens":100,"output_tokens":10,"cache_read_input_tokens":200,"cache_creation_input_tokens":50}}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"file body"}]}}
{"type":"assistant","message":{"model":"claude-opus-4-8","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":300,"output_tokens":20,"cache_read_input_tokens":400,"cache_creation_input_tokens":60}}}
`
	require.NoError(t, os.WriteFile(p, []byte(lines), 0o644))
	tokens, err := turnTokens(p)
	require.NoError(t, err)
	require.Len(t, tokens, 1)
	require.Equal(t, "claude-opus-4-8", tokens[0].Model)
	require.Equal(t, int64(400), tokens[0].Input, "input summed across the 2 assistant lines of the last turn")
	require.Equal(t, int64(30), tokens[0].Output)
	require.Equal(t, int64(600), tokens[0].CacheRead)
	require.Equal(t, int64(110), tokens[0].CacheCreation)
}

// TestTurnTokens_GroupsByModel verifies a turn spanning two models is split.
func TestTurnTokens_GroupsByModel(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.jsonl")
	lines := `{"type":"user","message":{"role":"user","content":"go"}}
{"type":"assistant","message":{"model":"claude-opus-4-8","content":[{"type":"text","text":"a"}],"usage":{"input_tokens":10,"output_tokens":1}}}
{"type":"assistant","message":{"model":"claude-haiku-4-5","content":[{"type":"text","text":"b"}],"usage":{"input_tokens":5,"output_tokens":2}}}
`
	require.NoError(t, os.WriteFile(p, []byte(lines), 0o644))
	tokens, err := turnTokens(p)
	require.NoError(t, err)
	require.Len(t, tokens, 2)
	got := map[string]session.TurnTokens{}
	for _, tk := range tokens {
		got[tk.Model] = tk
	}
	require.Equal(t, int64(10), got["claude-opus-4-8"].Input)
	require.Equal(t, int64(5), got["claude-haiku-4-5"].Input)
	require.Equal(t, int64(2), got["claude-haiku-4-5"].Output)
}

// TestTurnTokens_FromRealTranscript checks the committed two-turn fixture: only
// the final turn (PING) is metered.
func TestTurnTokens_FromRealTranscript(t *testing.T) {
	path := filepath.Join("testdata", "transcript.jsonl")
	if _, err := os.Stat(path); err != nil {
		t.Skip("spike fixture missing; run Task 1")
	}
	tokens, err := turnTokens(path)
	require.NoError(t, err)
	require.Len(t, tokens, 1)
	require.Equal(t, "claude-opus-4-8", tokens[0].Model)
	require.Equal(t, int64(2), tokens[0].Input, "only the last turn (PING) is summed")
	require.Equal(t, int64(4), tokens[0].Output)
	require.Equal(t, int64(17948), tokens[0].CacheRead)
	require.Equal(t, int64(1682), tokens[0].CacheCreation)
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
