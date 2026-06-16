package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
)

// hookPayload mirrors the real Stop-hook payload (Task-1 spike, v2.1.162).
// `last_assistant_message` carries the final text directly; there is NO
// `stop_reason` in the payload (it lives in the transcript).
type hookPayload struct {
	SessionID            string `json:"session_id"`
	TranscriptPath       string `json:"transcript_path"`
	LastAssistantMessage string `json:"last_assistant_message"`
}

// buildResult assembles the HookResult. FinalText comes from the payload's
// last_assistant_message (authoritative); the transcript is read only for
// `usage` (and as a fallback for text). Folds in /workspace/result.json if
// the agent wrote one.
func buildResult(payload []byte, resultJSONPath string) (session.HookResult, error) {
	var hp hookPayload
	if err := json.Unmarshal(payload, &hp); err != nil {
		return session.HookResult{}, fmt.Errorf("parse hook payload: %w", err)
	}
	res := session.HookResult{SessionID: hp.SessionID, FinalText: hp.LastAssistantMessage, TranscriptPath: hp.TranscriptPath}
	if hp.TranscriptPath != "" {
		if text, usage, stop, err := lastAssistantText(hp.TranscriptPath); err == nil {
			res.Usage = usage
			res.StopReason = stop
			if res.FinalText == "" {
				res.FinalText = text
			}
		}
	}
	if b, err := os.ReadFile(resultJSONPath); err == nil && json.Valid(b) {
		res.ResultJSON = b
	}
	return res, nil
}
