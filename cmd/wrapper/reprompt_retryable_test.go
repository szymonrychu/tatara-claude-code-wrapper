package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ISSUE tatara-operator#578, DEFECT C. The wrapper re-prompted on ANY is_error
// tool_result with "This is mandatory: call it again... Do not finish the turn
// until the call succeeds." Against a 4xx that instruction is unsatisfiable by
// construction - a client error means the identical call can never succeed - so
// it converted one doomed 400 ("this task owns no open MR", on a Task whose MR
// had already merged) into three identical re-submissions per turn, seven pod
// runs, three pod recreations, and a parked(no-outcome) Task.
//
// A 4xx must never carry the mandatory directive. A 5xx and an unclassifiable
// transport failure still must.
func TestRepromptDirective_MandatoryOnlyForRetryable(t *testing.T) {
	// The phrases that make a directive a RETRY LOOP. If any of them reaches an
	// agent whose call was refused 4xx, this defect is back.
	loopPhrases := []string{
		"do not finish the turn until the call succeeds",
		"with the same arguments",
	}

	tests := []struct {
		name          string
		errText       string
		wantRetryable bool
	}{
		{
			// THE LIVE TEXT from mt-i-mtg-decks-22.
			name:    "the operator refused a precondition",
			errText: `POST /tasks/mt-i-mtg-decks-22-3f4594d8ce81d4d0/outcome -> 400 {"error":"this task owns no open MR"}`,
		},
		{name: "a malformed payload", errText: "400: reason is required on every gate action"},
		{name: "a bare 400", errText: "400"},
		{name: "conflict", errText: "status 409: outcome in flight, retry"},
		{name: "operator crashed", errText: "500: internal error", wantRetryable: true},
		{
			// Classification is by STATUS CLASS, deliberately, not by a
			// hand-maintained list of the operator's individual failure modes -
			// which would drift the moment either side adds one. A 5xx that is in
			// truth permanent costs at most maxOutcomeReprompts wasted turns; a
			// 4xx read as retryable costs a burned Task.
			name:    "5xx is retryable even when the operator means it permanently",
			errText: "HTTP 507: task exceeds the byte budget", wantRetryable: true,
		},
		{name: "bad gateway from the forge", errText: "status code 502: scm read failed", wantRetryable: true},
		{name: "unavailable", errText: "-> 503 <html>", wantRetryable: true},
		{
			// No status at all: a transport failure is exactly when a retry is
			// right, and must never be downgraded to "your fault, do not retry".
			name: "transport failure with no status", wantRetryable: true,
			errText: "dial tcp 10.0.0.1:8080: connect: connection refused",
		},
		{
			name: "MCP framing error with no status", wantRetryable: true,
			errText: "MCP error -32603: request timed out after 30s",
		},
		{
			// A number in [400,599] that is NOT a status must not be mistaken for
			// one, or a genuinely retryable failure loses its retry.
			name: "an issue number is not a status", wantRetryable: true,
			errText: "connection reset while reporting issue #578",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.wantRetryable, retryableOutcomeFailure(tc.errText))

			got := strings.ToLower(repromptDirective("submit_outcome", tc.errText, retryableOutcomeFailure(tc.errText)))
			require.Contains(t, got, strings.ToLower(strings.TrimSpace(tc.errText)),
				"the directive must always quote the operator's own words back")

			if tc.wantRetryable {
				require.Contains(t, got, "do not finish the turn until the call succeeds",
					"a transient operator failure DOES succeed on retry; the directive must say so")
				return
			}
			for _, phrase := range loopPhrases {
				require.NotContains(t, got, phrase,
					"a 4xx must NOT be retried with a mandatory directive: the identical call can never succeed")
			}
			require.Contains(t, got, "do not resend it unchanged")
			require.Contains(t, got, "report_internal_issue",
				"the agent must be told it MAY stop, or it has no legal exit from an unsatisfiable precondition")
		})
	}
}
