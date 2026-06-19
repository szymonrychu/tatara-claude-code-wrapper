package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
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
