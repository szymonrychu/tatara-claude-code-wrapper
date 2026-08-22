package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

// operatorTurnCompletePayload mirrors the JSON fields tatara-operator's
// internal/controller.turnCompletePayload expects from the wrapper's
// turn-complete callback body (no shared Go module between the two repos -
// this is a wire-contract replica, hand-copied from turncallback.go, not an
// import).
type operatorTurnCompletePayload struct {
	TurnID          string  `json:"turnId"`
	TaskName        string  `json:"taskName,omitempty"`
	State           string  `json:"state"`
	FinalText       string  `json:"finalText"`
	StopReason      string  `json:"stopReason"`
	Error           string  `json:"error"`
	DurationSeconds float64 `json:"durationSeconds"`
}

// TestNewCallbackPayload_CarriesTaskNameAndDurationSeconds is the F8
// regression test: turn.Record has neither a taskName nor a durationSeconds
// field, so posting it raw leaves the operator's turnCompletePayload.TaskName
// and .DurationSeconds at their zero values. newCallbackPayload must build a
// wire body that, once decoded as the operator's turnCompletePayload,
// carries both correctly.
func TestNewCallbackPayload_CarriesTaskNameAndDurationSeconds(t *testing.T) {
	started := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	completed := started.Add(5*time.Second + 250*time.Millisecond)
	rec := &turn.Record{
		ID:          "turn-42",
		State:       turn.Complete,
		FinalText:   "done",
		StopReason:  "end_turn",
		StartedAt:   started,
		CompletedAt: &completed,
	}

	body, err := json.Marshal(newCallbackPayload(rec, "task-abc", nil))
	require.NoError(t, err)

	var p operatorTurnCompletePayload
	require.NoError(t, json.Unmarshal(body, &p))
	require.Equal(t, "turn-42", p.TurnID)
	require.Equal(t, "task-abc", p.TaskName, "taskName must reach the operator payload (bug F8)")
	require.InDelta(t, 5.25, p.DurationSeconds, 0.001, "durationSeconds must be CompletedAt-StartedAt (bug F8)")
	require.Equal(t, "done", p.FinalText)
	require.Equal(t, "end_turn", p.StopReason)
}

// TestAccountUsageWireKeys pins accountUsagePayload's exact JSON encoding.
// tatara-operator decodes these keys into its own
// internal/controller.turnAccountUsage - there is no shared Go module between
// the two repos, so this test IS the contract: it is the only thing that
// would catch a field rename here silently breaking the operator's decode.
func TestAccountUsageWireKeys(t *testing.T) {
	b, err := json.Marshal(accountUsagePayload{
		ObservedAt: time.Unix(0, 0).UTC(), FiveHourPercent: 1, FiveHourResetUnix: 2,
		WeeklyPercent: 3, WeeklyResetUnix: 4,
	})
	require.NoError(t, err)
	require.JSONEq(t,
		`{"observedAt":"1970-01-01T00:00:00Z","fiveHourPercent":1,"fiveHourResetUnix":2,"weeklyPercent":3,"weeklyResetUnix":4}`,
		string(b))
}

// TestNewCallbackPayload_ZeroDurationAndEmptyTaskNameWhenUnset verifies the
// zero-value defaults: a turn that has not completed yet (no CompletedAt)
// reports durationSeconds 0, and an empty taskName round-trips as an omitted
// (not synthesized) field.
func TestNewCallbackPayload_ZeroDurationAndEmptyTaskNameWhenUnset(t *testing.T) {
	rec := &turn.Record{ID: "turn-1", State: turn.Running, StartedAt: time.Now()}

	body, err := json.Marshal(newCallbackPayload(rec, "", nil))
	require.NoError(t, err)

	var p operatorTurnCompletePayload
	require.NoError(t, json.Unmarshal(body, &p))
	require.Equal(t, float64(0), p.DurationSeconds)
	require.Equal(t, "", p.TaskName)
}
