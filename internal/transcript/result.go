package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// TurnTokens is the per-model token total for one turn, summed from the
// persisted transcript. The stop hook computes it at completion; the crash
// recovery path computes it when synthesizing a completion from the restored
// transcript. Lives here (not in session) so transcript parsing stays free of
// a session dependency; session.TurnTokens aliases this type.
type TurnTokens struct {
	Model         string `json:"model"`
	Input         int64  `json:"input"`
	Output        int64  `json:"output"`
	CacheRead     int64  `json:"cacheRead"`
	CacheCreation int64  `json:"cacheCreation"`
}

// roleLine is the content-agnostic view of a transcript line used by LastMessage.
// The top-level "type" is the conversation role ("user"/"assistant") for message
// lines; content is deliberately not decoded because genuine user prompts carry a
// string content while assistant/tool lines carry an array, and we only need the
// role and (for assistant lines) the stop_reason.
type roleLine struct {
	Type    string `json:"type"`
	Message *struct {
		StopReason string `json:"stop_reason"`
	} `json:"message"`
}

// LastMessage returns the role and stop_reason of the last conversation message
// in the JSONL transcript at path. Non-message lines (system, summary) are
// skipped so the result reflects the last actual message. role is "" when the
// transcript has no message lines. This is a one-shot synchronous read, distinct
// from the streaming Tailer, used at crash-resume time before any hook lands.
func LastMessage(path string) (role, stopReason string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", fmt.Errorf("open transcript: %w", err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), maxPartialBytes)
	for sc.Scan() {
		var rl roleLine
		if err := json.Unmarshal(sc.Bytes(), &rl); err != nil {
			continue
		}
		if rl.Type != "user" && rl.Type != "assistant" {
			continue
		}
		role = rl.Type
		if rl.Message != nil {
			stopReason = rl.Message.StopReason
		} else {
			stopReason = ""
		}
	}
	if err := sc.Err(); err != nil {
		return "", "", fmt.Errorf("scan transcript: %w", err)
	}
	return role, stopReason, nil
}

type assistantLine struct {
	Type    string `json:"type"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage      json.RawMessage `json:"usage"`
		StopReason string          `json:"stop_reason"`
	} `json:"message"`
}

// LastAssistant returns the concatenated text blocks of the final assistant
// line in a JSONL transcript, plus its usage object and stop_reason.
func LastAssistant(path string) (string, json.RawMessage, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil, "", fmt.Errorf("open transcript: %w", err)
	}
	defer func() { _ = f.Close() }()

	var lastText string
	var lastUsage json.RawMessage
	var lastStop string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), maxPartialBytes)
	for sc.Scan() {
		line := sc.Bytes()
		var al assistantLine
		if err := json.Unmarshal(line, &al); err != nil || al.Type != "assistant" {
			continue
		}
		text := ""
		for _, c := range al.Message.Content {
			if c.Type == "text" {
				text += c.Text
			}
		}
		lastUsage, lastStop = al.Message.Usage, al.Message.StopReason
		if text != "" {
			lastText = text
		}
	}
	if err := sc.Err(); err != nil {
		return "", nil, "", fmt.Errorf("scan transcript: %w", err)
	}
	return lastText, lastUsage, lastStop, nil
}

// turnLine is the subset of a transcript JSONL envelope needed to sum per-turn
// token usage. A genuine user prompt has a string `content` and marks a turn
// boundary; tool results arrive as user lines too but carry an array `content`,
// so they do not reset the accumulator.
type turnLine struct {
	Type    string `json:"type"`
	Message struct {
		Content json.RawMessage `json:"content"`
		Model   string          `json:"model"`
		Usage   struct {
			Input         int64 `json:"input_tokens"`
			Output        int64 `json:"output_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// SumTurnTokens sums token usage across every assistant message of the LAST turn
// in the transcript, grouped by model. The transcript accumulates across all
// turns of a session, so the accumulator resets at each typed user prompt;
// only the final turn's assistant lines survive into the result. Returning the
// summed view (not the single last-message usage) is what makes the token
// metric correct for multi-step agentic turns.
func SumTurnTokens(path string) ([]TurnTokens, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open transcript: %w", err)
	}
	defer func() { _ = f.Close() }()

	byModel := map[string]*TurnTokens{}
	var order []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), maxPartialBytes)
	for sc.Scan() {
		var tl turnLine
		if err := json.Unmarshal(sc.Bytes(), &tl); err != nil {
			continue
		}
		if tl.Type == "user" && isJSONString(tl.Message.Content) {
			// New turn boundary: drop everything accumulated for prior turns.
			byModel = map[string]*TurnTokens{}
			order = order[:0]
			continue
		}
		if tl.Type != "assistant" {
			continue
		}
		t := byModel[tl.Message.Model]
		if t == nil {
			t = &TurnTokens{Model: tl.Message.Model}
			byModel[tl.Message.Model] = t
			order = append(order, tl.Message.Model)
		}
		t.Input += tl.Message.Usage.Input
		t.Output += tl.Message.Usage.Output
		t.CacheRead += tl.Message.Usage.CacheRead
		t.CacheCreation += tl.Message.Usage.CacheCreation
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan transcript: %w", err)
	}
	out := make([]TurnTokens, 0, len(order))
	for _, m := range order {
		out = append(out, *byModel[m])
	}
	return out, nil
}

// isJSONString reports whether a raw JSON value is a string literal.
func isJSONString(raw json.RawMessage) bool {
	b := bytes.TrimSpace(raw)
	return len(b) > 0 && b[0] == '"'
}

// criticalOutcomeTools is the set of MCP outcome tools whose rejection by the
// operator must be propagated back to the agent (a silently dropped decline is
// what produced the false "refused-no-explanation" park). Matched on the bare
// tool name after stripping any MCP namespace prefix (mcp__<server>__<name>).
//
// It named decline_implementation and already_done until 2026-07-28. Both were
// retired from tatara-cli (asserted gone at
// tatara-cli/internal/mcp/tools_test.go:155-156), so the whole re-prompt path
// was dead. submit_outcome is the tool that actually terminates a Task and the
// only one whose rejection the agent must see.
var criticalOutcomeTools = map[string]bool{
	"submit_outcome": true,
}

// bareToolName strips an MCP namespace prefix ("mcp__tatara__decline_implementation"
// -> "decline_implementation"). A non-prefixed name is returned unchanged.
func bareToolName(name string) string {
	if i := strings.LastIndex(name, "__"); i >= 0 {
		return name[i+2:]
	}
	return name
}

// outcomeLine is the minimal view of a transcript line carrying tool_use and
// tool_result blocks, used by FailedCriticalOutcome. content is decoded as an
// array of blocks; genuine string-content user prompts simply fail to unmarshal
// into the array and are skipped.
type outcomeLine struct {
	Type    string `json:"type"`
	Message *struct {
		Content []struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			ToolUseID string          `json:"tool_use_id"`
			IsError   bool            `json:"is_error"`
			Content   json.RawMessage `json:"content"`
		} `json:"content"`
	} `json:"message"`
}

// FailedCriticalOutcome scans the JSONL transcript at path for a critical-outcome
// MCP tool call (submit_outcome, under any MCP namespace prefix) whose
// tool_result came back is_error:true. It returns the bare tool name, the
// operator's error text, and found=true on a hit. found=false (nil error) is
// the common no-failure case. A later non-error result for the same
// tool_use_id supersedes an earlier failure (the agent already corrected).
func FailedCriticalOutcome(path string) (tool, errText string, found bool, err error) {
	f, oerr := os.Open(path)
	if oerr != nil {
		return "", "", false, fmt.Errorf("open transcript: %w", oerr)
	}
	defer func() { _ = f.Close() }()

	// toolUseByID maps tool_use_id -> bare critical tool name for in-flight calls.
	toolUseByID := map[string]string{}
	// lastResult tracks the most recent outcome (errText or "") per bare tool
	// name. An empty string means the last result was a success; non-empty means
	// the last result was a failure with that error text. A later successful retry
	// (different tool_use_id, same bare name) clears the failure.
	lastResult := map[string]string{}
	lastFailed := map[string]bool{}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), maxPartialBytes)
	for sc.Scan() {
		var ol outcomeLine
		if jerr := json.Unmarshal(sc.Bytes(), &ol); jerr != nil || ol.Message == nil {
			continue
		}
		for _, b := range ol.Message.Content {
			switch b.Type {
			case "tool_use":
				if name := bareToolName(b.Name); criticalOutcomeTools[name] && b.ID != "" {
					toolUseByID[b.ID] = name
				}
			case "tool_result":
				if b.ToolUseID == "" {
					continue
				}
				name, ok := toolUseByID[b.ToolUseID]
				if !ok {
					continue // not a critical outcome tool call
				}
				if b.IsError {
					lastResult[name] = jsonContentString(b.Content)
					lastFailed[name] = true
				} else {
					// A later success for the same tool name clears any earlier failure.
					lastResult[name] = ""
					lastFailed[name] = false
				}
			}
		}
	}
	if serr := sc.Err(); serr != nil {
		return "", "", false, fmt.Errorf("scan transcript: %w", serr)
	}

	for name, isF := range lastFailed {
		if isF {
			return name, lastResult[name], true, nil
		}
	}
	return "", "", false, nil
}

// jsonContentString renders a tool_result content field as a human-readable
// string. The field is either a JSON string ("400: ...") or an array of blocks;
// in the array case the raw JSON is returned so the message is never empty.
func jsonContentString(raw json.RawMessage) string {
	if isJSONString(raw) {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return string(raw)
}

// httpStatusMarkers are the tokens an operator/MCP error text puts immediately
// before the numeric status. Anchoring on them is what keeps a bare number that
// happens to fall in [400,599] - an issue number like "#578", a line number, a
// port - from being read as a status.
var httpStatusMarkers = []string{"http/1.1", "http", "status code", "status", "code", "->", "returned"}

// OutcomeErrorStatus extracts the HTTP status an outcome tool_result error text
// reports, if it reports one at all (issue tatara-operator#578).
//
// IT EXISTS TO SEPARATE RETRYABLE FROM NON-RETRYABLE. A 4xx is a CLIENT error:
// the identical call can never succeed, so re-prompting the agent to "call it
// again until it succeeds" converts one bad response into a turn loop and,
// through the operator's pod-recreation budget, into a burned Task. A 5xx or a
// transport failure genuinely does succeed on retry. Nothing downstream of the
// tool call can tell the two apart except this text.
//
// It recognises the status only where an HTTP status actually appears:
//   - at the very START of the text ("400: reason is required", "400")
//   - immediately after an httpStatusMarkers token ("status 502", "-> 400 {...}")
//
// ok is false when no status is present, which callers MUST treat as
// retryable/unknown rather than as a client error: an unparsed transport failure
// must not be silently downgraded to "do not retry".
func OutcomeErrorStatus(errText string) (int, bool) {
	s := strings.ToLower(strings.TrimSpace(errText))
	if code, ok := leadingStatus(s); ok {
		return code, true
	}
	for i := 0; i < len(s); i++ {
		// THE MARKER MUST START A WORD. Without this, "code" and "returned" match
		// as SUFFIXES of ordinary words - "scanned barcode 404 not found",
		// "failed to decode 500 bytes", "unreturned 502 after retry" - and each
		// one is read as an HTTP status. That false positive is this fix's own
		// failure mode running backwards: a bogus 4xx strips the mandatory retry
		// directive off a failure that was genuinely retryable.
		if i > 0 && isWordByte(s[i-1]) {
			continue
		}
		for _, marker := range httpStatusMarkers {
			if !strings.HasPrefix(s[i:], marker) {
				continue
			}
			rest := strings.TrimLeft(s[i+len(marker):], " :=\t")
			if code, ok := leadingStatus(rest); ok {
				return code, true
			}
		}
	}
	return 0, false
}

// isWordByte reports whether b can be part of a word, for the boundary check
// above. ASCII is enough: every marker is ASCII, so only an ASCII byte can be
// the tail of a word that swallows one.
func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_'
}

// leadingStatus reads a standalone 3-digit status in [400,599] off the front of
// s. The digits must not run on into a fourth (so "4001" and a token like
// "5000ms" are refused) and must be exactly three long.
func leadingStatus(s string) (int, bool) {
	if len(s) < 3 {
		return 0, false
	}
	for i := range 3 {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	if len(s) > 3 && s[3] >= '0' && s[3] <= '9' {
		return 0, false
	}
	code := int(s[0]-'0')*100 + int(s[1]-'0')*10 + int(s[2]-'0')
	if code < 400 || code > 599 {
		return 0, false
	}
	return code, true
}
