package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBuildResult_StopReasonFromTranscript verifies buildResult extracts the
// stop_reason from the transcript (the hook payload carries no stop_reason).
// The transcript parsing itself is covered in internal/transcript.
func TestBuildResult_StopReasonFromTranscript(t *testing.T) {
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	lines := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello"}],"usage":{"output_tokens":5},"stop_reason":"end_turn"}}
`
	require.NoError(t, os.WriteFile(transcriptPath, []byte(lines), 0o644))

	payload := []byte(`{"session_id":"sess-1","transcript_path":"` + transcriptPath + `","last_assistant_message":"hello"}`)
	res, err := buildResult(payload, "/nonexistent/result.json")
	require.NoError(t, err)
	require.Equal(t, "end_turn", res.StopReason, "StopReason must be extracted from transcript")
}
