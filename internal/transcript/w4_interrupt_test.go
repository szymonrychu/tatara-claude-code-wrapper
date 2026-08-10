package transcript

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWasInterrupted_Fixtures pins the two literals the CLI actually writes and
// the one case that must NOT match.
//
// The fixtures are transcribed from a real CLI 2.1.226 session in which a turn
// was ESC'd mid-Bash-call: the interrupted turn ends with the rejected
// tool_result followed by the marker, and NOTHING ELSE - no end_turn assistant
// message, no stop_reason, no Stop hook. That absence is the whole reason this
// function exists, so the fixtures keep it exactly.
func TestWasInterrupted_Fixtures(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
		want bool
		why  string
	}{
		{
			name: "esc during a tool call",
			file: "interrupted_tool_use.jsonl",
			want: true,
			why:  "trailing '[Request interrupted by user for tool use]' is the terminal marker for the common case: ESC while a tool was running",
		},
		{
			name: "esc while streaming text",
			file: "interrupted_plain.jsonl",
			want: true,
			why:  "the CLI drops the ' for tool use' suffix when no tool was in flight; both variants are the same event",
		},
		{
			name: "interrupt earlier in the session, then a fresh turn",
			file: "interrupted_then_resumed.jsonl",
			want: false,
			why:  "an ESC in the conversation's HISTORY must never resolve the turn now in flight - only the last conversation entry counts",
		},
		{
			name: "ordinary completed turn",
			file: "not_interrupted.jsonl",
			want: false,
			why:  "a turn that ended with end_turn is not an interruption",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, WasInterrupted(filepath.Join("testdata", tc.file)), tc.why)
		})
	}
}

// TestWasInterrupted_MissingFileIsNotInterrupted: the poller calls this before
// the transcript exists (the file is named after a session id the CLI has not
// minted yet). Absence of evidence must never resolve a turn.
func TestWasInterrupted_MissingFileIsNotInterrupted(t *testing.T) {
	require.False(t, WasInterrupted(filepath.Join(t.TempDir(), "nope.jsonl")))
	require.False(t, WasInterrupted(""))
}

// TestWasInterrupted_IgnoresNonMessageEnvelopes guards the skip list. Claude
// Code writes a file-history-snapshot and an attachment AFTER the marker when
// the next prompt arrives; if either counted as a conversation entry the
// verdict would be cleared and a genuinely interrupted turn would wedge.
func TestWasInterrupted_IgnoresNonMessageEnvelopes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	lines := `{"type":"user","message":{"role":"user","content":"go"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"working"}],"stop_reason":"tool_use"}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]},"interruptedMessageId":"msg_1"}
{"type":"file-history-snapshot","messageId":"m1","snapshot":{"messageId":"m1","trackedFileBackups":{}}}
{"type":"system","subtype":"turn_duration","durationMs":1599}
{"type":"summary","summary":"a summary"}
`
	require.NoError(t, os.WriteFile(p, []byte(lines), 0o644))
	require.True(t, WasInterrupted(p),
		"system/summary/file-history-snapshot entries after the marker are envelopes, not conversation")
}

// TestWasInterrupted_TypedPromptAfterMarkerClears is the inverse: a real user
// message following the marker means the interruption has already been dealt
// with and a new turn is under way.
func TestWasInterrupted_TypedPromptAfterMarkerClears(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	lines := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]},"interruptedMessageId":"msg_1"}
{"type":"user","message":{"role":"user","content":"carry on"}}
`
	require.NoError(t, os.WriteFile(p, []byte(lines), 0o644))
	require.False(t, WasInterrupted(p))
}

// TestWasInterrupted_PrefixNotSubstring: the marker is what the CLI writes, not
// something an agent can say. A model quoting the phrase mid-sentence in its own
// output must not resolve the turn, and neither must a tool_result containing it.
func TestWasInterrupted_PrefixNotSubstring(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	lines := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"I saw [Request interrupted by user] in the log"}],"stop_reason":"end_turn"}}
`
	require.NoError(t, os.WriteFile(p, []byte(lines), 0o644))
	require.False(t, WasInterrupted(p), "assistant output is never the marker, whatever it quotes")

	p2 := filepath.Join(t.TempDir(), "t2.jsonl")
	lines2 := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"note: [Request interrupted by user] appeared earlier"}]}}
`
	require.NoError(t, os.WriteFile(p2, []byte(lines2), 0o644))
	require.False(t, WasInterrupted(p2), "the marker is a whole text block, matched from its first byte")
}
