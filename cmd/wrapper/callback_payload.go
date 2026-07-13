package main

import (
	"encoding/json"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

// callbackPayload is the wire body POSTed to the operator's
// /internal/turn-complete endpoint (bug F8). turn.Record cannot be posted
// raw: it carries neither taskName nor durationSeconds, both of which
// tatara-operator's internal/controller.turnCompletePayload requires (the
// former for O(1) task resolution, the latter for the turn-duration metric).
// JSON tags must match turnCompletePayload exactly - there is no shared Go
// module between the two repos.
type callbackPayload struct {
	TurnID          string                     `json:"turnId"`
	TaskName        string                     `json:"taskName,omitempty"`
	State           turn.State                 `json:"state"`
	FinalText       string                     `json:"finalText,omitempty"`
	StopReason      string                     `json:"stopReason,omitempty"`
	Error           string                     `json:"error,omitempty"`
	DurationSeconds float64                    `json:"durationSeconds"`
	Usage           json.RawMessage            `json:"usage,omitempty"`
	PushedRepos     []string                   `json:"pushedRepos,omitempty"`
	InternalIssues  []turn.InternalIssueReport `json:"internalIssues,omitempty"`
}

// newCallbackPayload builds the turn-complete callback body from rec plus
// taskName (sourced from config.TaskName, i.e. the TATARA_TASK env var the
// operator injects). durationSeconds is CompletedAt-StartedAt; zero when the
// turn has not completed (CompletedAt nil).
func newCallbackPayload(rec *turn.Record, taskName string) callbackPayload {
	var dur float64
	if rec.CompletedAt != nil {
		dur = rec.CompletedAt.Sub(rec.StartedAt).Seconds()
	}
	return callbackPayload{
		TurnID:          rec.ID,
		TaskName:        taskName,
		State:           rec.State,
		FinalText:       rec.FinalText,
		StopReason:      rec.StopReason,
		Error:           rec.Error,
		DurationSeconds: dur,
		Usage:           rec.Usage,
		PushedRepos:     rec.PushedRepos,
		InternalIssues:  rec.InternalIssues,
	}
}
