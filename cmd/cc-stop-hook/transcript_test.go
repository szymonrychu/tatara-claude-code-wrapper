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
