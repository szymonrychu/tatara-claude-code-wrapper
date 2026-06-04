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
{"type":"assistant","message":{"content":[{"type":"text","text":"first"}],"usage":{"output_tokens":1}}}
{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"final answer"}],"usage":{"output_tokens":2}}}
`
	require.NoError(t, os.WriteFile(p, []byte(lines), 0o644))
	text, usage, err := lastAssistantText(p)
	require.NoError(t, err)
	require.Equal(t, "final answer", text)
	require.JSONEq(t, `{"output_tokens":2}`, string(usage))
}

func TestLastAssistantText_FromRealTranscript(t *testing.T) {
	path := filepath.Join("testdata", "transcript.jsonl")
	if _, err := os.Stat(path); err != nil {
		t.Skip("spike fixture missing; run Task 1")
	}
	text, usage, err := lastAssistantText(path)
	require.NoError(t, err)
	require.NotEmpty(t, text)
	_ = usage // usage may be present; type is json.RawMessage
}
