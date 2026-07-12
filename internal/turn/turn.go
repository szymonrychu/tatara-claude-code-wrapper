// Package turn holds per-turn records and a thread-safe store.
package turn

import (
	"encoding/json"
	"time"
)

type State string

const (
	Running  State = "running"
	Complete State = "complete"
	Failed   State = "failed"
)

// Record is one user turn and its eventual result.
type Record struct {
	ID             string          `json:"turnId"`
	State          State           `json:"state"`
	Text           string          `json:"-"`
	CallbackURL    string          `json:"-"`
	FinalText      string          `json:"finalText,omitempty"`
	ResultJSON     json.RawMessage `json:"resultJson,omitempty"`
	Usage          json.RawMessage `json:"usage,omitempty"`
	StopReason     string          `json:"stopReason,omitempty"`
	Error          string          `json:"error,omitempty"`
	StartedAt      time.Time       `json:"startedAt"`
	LastActivityAt time.Time       `json:"lastActivityAt"`
	CompletedAt    *time.Time      `json:"completedAt,omitempty"`
	// SessionID and ConversationObjectKey report the persisted conversation
	// pointer back to the operator (issue #114) so it records them on the Task
	// Status and replays them (CONVERSATION_SESSION_ID) on the next-phase pod.
	// Set by the app's turn finaliser only when conversation persistence is on.
	SessionID             string `json:"sessionId,omitempty"`
	ConversationObjectKey string `json:"conversationObjectKey,omitempty"`
	// PushedRepos is the set of project repos this turn actually committed and
	// pushed (had a diff). Reported to the operator on the callback so it knows
	// which repos were touched in a multi-repo task (Defect A). Empty/absent for
	// a turn that pushed nothing or a single-repo task with no diff.
	PushedRepos []string `json:"pushedRepos,omitempty"`
	// InternalIssues is the set of report_internal_issue calls the agent made
	// during this turn, drained from the transcript Tailer's per-turn
	// accumulator by cmd/wrapper/app.go before delivery. Empty/absent when the
	// turn reported nothing (the common case).
	InternalIssues []InternalIssueReport `json:"internalIssues,omitempty"`
}

// InternalIssueReport is one report_internal_issue tool call from the turn's
// transcript, drained from the transcript Tailer's per-turn accumulator and
// attached to the Record so the operator's /internal/turn-complete callback
// receives it (agent pods are not Loki-scraped; the operator's collected
// stdout is the only place this can become alertable). Category and Severity
// are the tailer's already-clamped values (see internal/transcript/tailer.go
// knownIssueCategories/knownIssueSeverities), not the raw agent input, so a
// missing/unknown severity still resolves to exactly "error" or "warn".
// JSON tags must match tatara-operator's internal/controller.InternalIssueReport
// exactly - there is no shared Go module between the two repos.
type InternalIssueReport struct {
	Category      string `json:"category"`
	Severity      string `json:"severity"`
	Description   string `json:"description"`
	OffendingTool string `json:"offending_tool"`
	ResourceID    string `json:"resource_id"`
}

// Summary is the compact form returned by List.
type Summary struct {
	ID             string     `json:"turnId"`
	State          State      `json:"state"`
	StartedAt      time.Time  `json:"startedAt"`
	LastActivityAt time.Time  `json:"lastActivityAt"`
	CompletedAt    *time.Time `json:"completedAt,omitempty"`
}
