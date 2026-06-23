package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLastAssistant_Synthetic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.jsonl")
	lines := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"first"}],"usage":{"output_tokens":1},"stop_reason":"tool_use"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"final answer"}],"usage":{"output_tokens":2},"stop_reason":"end_turn"}}
`
	require.NoError(t, os.WriteFile(p, []byte(lines), 0o644))
	text, usage, stop, err := LastAssistant(p)
	require.NoError(t, err)
	require.Equal(t, "final answer", text)
	require.JSONEq(t, `{"output_tokens":2}`, string(usage))
	require.Equal(t, "end_turn", stop)
}

// TestLastAssistant_ToolOnlyFinalLine verifies that when the final assistant
// turn ends with a tool_use block (no text content) the stop_reason and usage
// from THAT final line are returned, not the values from an earlier text-bearing
// assistant line.
func TestLastAssistant_ToolOnlyFinalLine(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.jsonl")
	lines := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"let me check"}],"usage":{"output_tokens":5},"stop_reason":"end_turn"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}],"usage":{"output_tokens":10},"stop_reason":"tool_use"}}
`
	require.NoError(t, os.WriteFile(p, []byte(lines), 0o644))
	text, usage, stop, err := LastAssistant(p)
	require.NoError(t, err)
	require.Equal(t, "let me check", text)
	require.Equal(t, "tool_use", stop, "stop_reason must be from the final assistant line, not from an earlier text-bearing one")
	require.JSONEq(t, `{"output_tokens":10}`, string(usage), "usage must be from the final assistant line")
}

// TestLastMessage_RoleAndStopReason verifies LastMessage reports the role and
// stop_reason of the last conversation message, skipping non-message lines.
func TestLastMessage_RoleAndStopReason(t *testing.T) {
	dir := t.TempDir()

	// Completed turn: last message is a terminal assistant answer.
	done := filepath.Join(dir, "done.jsonl")
	require.NoError(t, os.WriteFile(done, []byte(
		`{"type":"user","message":{"role":"user","content":"go"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}}
{"type":"summary","summary":"trailing non-message line"}
`), 0o644))
	role, stop, err := LastMessage(done)
	require.NoError(t, err)
	require.Equal(t, "assistant", role, "trailing summary line must be skipped")
	require.Equal(t, "end_turn", stop)

	// Pending turn: last message is the restored user prompt.
	pending := filepath.Join(dir, "pending.jsonl")
	require.NoError(t, os.WriteFile(pending, []byte(
		`{"type":"user","message":{"role":"user","content":"keep going"}}
`), 0o644))
	role, stop, err = LastMessage(pending)
	require.NoError(t, err)
	require.Equal(t, "user", role)
	require.Equal(t, "", stop)

	// Mid-turn tool step: last message is an assistant tool_use awaiting a tool.
	mid := filepath.Join(dir, "mid.jsonl")
	require.NoError(t, os.WriteFile(mid, []byte(
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}],"stop_reason":"tool_use"}}
`), 0o644))
	role, stop, err = LastMessage(mid)
	require.NoError(t, err)
	require.Equal(t, "assistant", role)
	require.Equal(t, "tool_use", stop)
}

// TestLastMessage_NoMessages returns an empty role for a transcript with no
// message-bearing lines.
func TestLastMessage_NoMessages(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.jsonl")
	require.NoError(t, os.WriteFile(p, []byte(`{"type":"system","content":"boot"}`+"\n"), 0o644))
	role, stop, err := LastMessage(p)
	require.NoError(t, err)
	require.Equal(t, "", role)
	require.Equal(t, "", stop)
}

// TestSumTurnTokens_SumsCurrentTurnOnly verifies that token usage is summed across
// every assistant line of the LAST turn, that a typed user prompt resets the
// accumulator (prior turns are excluded), and that a tool_result user line
// (array content) does NOT reset it.
func TestSumTurnTokens_SumsCurrentTurnOnly(t *testing.T) {
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
	tokens, err := SumTurnTokens(p)
	require.NoError(t, err)
	require.Len(t, tokens, 1)
	require.Equal(t, "claude-opus-4-8", tokens[0].Model)
	require.Equal(t, int64(400), tokens[0].Input, "input summed across the 2 assistant lines of the last turn")
	require.Equal(t, int64(30), tokens[0].Output)
	require.Equal(t, int64(600), tokens[0].CacheRead)
	require.Equal(t, int64(110), tokens[0].CacheCreation)
}

// TestSumTurnTokens_GroupsByModel verifies a turn spanning two models is split.
func TestSumTurnTokens_GroupsByModel(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.jsonl")
	lines := `{"type":"user","message":{"role":"user","content":"go"}}
{"type":"assistant","message":{"model":"claude-opus-4-8","content":[{"type":"text","text":"a"}],"usage":{"input_tokens":10,"output_tokens":1}}}
{"type":"assistant","message":{"model":"claude-haiku-4-5","content":[{"type":"text","text":"b"}],"usage":{"input_tokens":5,"output_tokens":2}}}
`
	require.NoError(t, os.WriteFile(p, []byte(lines), 0o644))
	tokens, err := SumTurnTokens(p)
	require.NoError(t, err)
	require.Len(t, tokens, 2)
	got := map[string]TurnTokens{}
	for _, tk := range tokens {
		got[tk.Model] = tk
	}
	require.Equal(t, int64(10), got["claude-opus-4-8"].Input)
	require.Equal(t, int64(5), got["claude-haiku-4-5"].Input)
	require.Equal(t, int64(2), got["claude-haiku-4-5"].Output)
}

// TestSumTurnTokens_FromRealTranscript checks the committed two-turn fixture:
// only the final turn (PING) is metered.
func TestSumTurnTokens_FromRealTranscript(t *testing.T) {
	path := filepath.Join("testdata", "transcript.jsonl")
	if _, err := os.Stat(path); err != nil {
		t.Skip("fixture missing")
	}
	tokens, err := SumTurnTokens(path)
	require.NoError(t, err)
	require.Len(t, tokens, 1)
	require.Equal(t, "claude-opus-4-8", tokens[0].Model)
	require.Equal(t, int64(2), tokens[0].Input, "only the last turn (PING) is summed")
	require.Equal(t, int64(4), tokens[0].Output)
	require.Equal(t, int64(17948), tokens[0].CacheRead)
	require.Equal(t, int64(1682), tokens[0].CacheCreation)
}

func TestLastAssistant_FromRealTranscript(t *testing.T) {
	path := filepath.Join("testdata", "transcript.jsonl")
	if _, err := os.Stat(path); err != nil {
		t.Skip("fixture missing")
	}
	text, usage, stop, err := LastAssistant(path)
	require.NoError(t, err)
	require.NotEmpty(t, text)
	require.Equal(t, "end_turn", stop)
	_ = usage
}

// TestFailedCriticalOutcome covers the rejected-decline detection that the
// wrapper uses to re-prompt the agent instead of finishing a turn silently.
func TestFailedCriticalOutcome(t *testing.T) {
	tests := []struct {
		name       string
		lines      []string
		wantTool   string
		wantErrSub string
		wantFound  bool
	}{
		{
			name: "rejected decline_implementation",
			lines: []string{
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"mcp__tatara__decline_implementation","input":{"reason":"   "}}]}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"400: reason must not be blank"}]}}`,
			},
			wantTool:   "decline_implementation",
			wantErrSub: "reason must not be blank",
			wantFound:  true,
		},
		{
			name: "rejected already_done",
			lines: []string{
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"a9","name":"mcp__tatara__already_done","input":{}}]}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"a9","is_error":true,"content":"400: reason required"}]}}`,
			},
			wantTool:   "already_done",
			wantErrSub: "reason required",
			wantFound:  true,
		},
		{
			name: "successful decline is not a hit",
			lines: []string{
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"mcp__tatara__decline_implementation","input":{"reason":"blocked on creds"}}]}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":false,"content":"ok"}]}}`,
			},
			wantFound: false,
		},
		{
			name: "unrelated failing tool is not a hit",
			lines: []string{
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"b1","name":"Bash","input":{"command":"false"}}]}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"b1","is_error":true,"content":"exit 1"}]}}`,
			},
			wantFound: false,
		},
		{
			name: "later successful retry supersedes earlier failure",
			lines: []string{
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"mcp__tatara__decline_implementation","input":{"reason":""}}]}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"400"}]}}`,
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"mcp__tatara__decline_implementation","input":{"reason":"real"}}]}}`,
				`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","is_error":false,"content":"ok"}]}}`,
			},
			wantFound: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "t.jsonl")
			require.NoError(t, os.WriteFile(path, []byte(strings.Join(tt.lines, "\n")+"\n"), 0o644))

			tool, errText, found, err := FailedCriticalOutcome(path)
			require.NoError(t, err)
			require.Equal(t, tt.wantFound, found)
			if tt.wantFound {
				require.Equal(t, tt.wantTool, tool)
				require.Contains(t, errText, tt.wantErrSub)
			}
		})
	}
}

func TestFailedCriticalOutcome_MissingFileErrors(t *testing.T) {
	_, _, _, err := FailedCriticalOutcome(filepath.Join(t.TempDir(), "nope.jsonl"))
	require.Error(t, err)
}
