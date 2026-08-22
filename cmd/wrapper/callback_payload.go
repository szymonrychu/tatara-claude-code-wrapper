package main

import (
	"encoding/json"
	"time"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

// accountUsagePayload is the account-wide Claude subscription usage snapshot
// this pod last observed via the statusline (cmd/cc-statusline). It is NOT
// per-turn: it describes the shared subscription every agent pod in the fleet
// burns. JSON tags must match tatara-operator's internal/controller
// .turnAccountUsage exactly - there is no shared Go module between the repos.
//
// The key is "accountUsage", deliberately NOT the retired #225 "rateLimit":
// a current operator explicitly ignores "rateLimit", so reusing it would make
// old-vs-new decoding ambiguous.
type accountUsagePayload struct {
	ObservedAt        time.Time `json:"observedAt"`
	FiveHourPercent   float64   `json:"fiveHourPercent"`
	FiveHourResetUnix int64     `json:"fiveHourResetUnix,omitempty"`
	WeeklyPercent     float64   `json:"weeklyPercent"`
	WeeklyResetUnix   int64     `json:"weeklyResetUnix,omitempty"`
}

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
	FailedRepos     []string                   `json:"failedRepos,omitempty"`
	InternalIssues  []turn.InternalIssueReport `json:"internalIssues,omitempty"`
	// AccountUsage is omitted ENTIRELY when this pod has observed no snapshot.
	// claude reports rate_limits only after the session's first API response,
	// so a pod's first callbacks legitimately carry none - and an omitted key
	// is the only encoding of that: a zero-valued object would read to the
	// operator's admission gate as "0% used".
	AccountUsage *accountUsagePayload `json:"accountUsage,omitempty"`
}

// newCallbackPayload builds the turn-complete callback body from rec plus
// taskName (sourced from config.TaskName, i.e. the TATARA_TASK env var the
// operator injects). durationSeconds is CompletedAt-StartedAt; zero when the
// turn has not completed (CompletedAt nil).
func newCallbackPayload(rec *turn.Record, taskName string, usage *accountUsagePayload) callbackPayload {
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
		FailedRepos:     rec.FailedRepos,
		InternalIssues:  rec.InternalIssues,
		AccountUsage:    usage,
	}
}
