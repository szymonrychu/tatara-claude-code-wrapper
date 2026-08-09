package transcript

import "testing"

// OutcomeErrorStatus is what separates a retryable outcome failure from a
// non-retryable one (tatara-operator#578), so both a MISSED status (a 4xx read
// as retryable, which loops) and a FALSE one (a transport failure read as a
// client error, which loses a legitimate retry) are defects.
func TestOutcomeErrorStatus(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
		ok   bool
	}{
		{"leading with a colon", "400: reason is required", 400, true},
		{"leading bare", "400", 400, true},
		{"leading with text", "404 not found", 404, true},
		{"the live arrow form", `POST /tasks/t/outcome -> 400 {"error":"this task owns no open MR"}`, 400, true},
		{"status word", "status 409: outcome in flight, retry", 409, true},
		{"status colon", "request failed, status: 502", 502, true},
		{"status code words", "status code 503", 503, true},
		{"http prefix", "HTTP 500 internal error", 500, true},
		{"returned", "the operator returned 507", 507, true},
		{"upper case", "Status 429 Too Many Requests", 429, true},
		{"no status at all", "dial tcp: connection refused", 0, false},
		{"an issue number is not a status", "while reporting issue #578", 0, false},
		{"a four-digit number is not a status", "4001 items", 0, false},
		{"a 2xx is not an error status", "200 ok", 0, false},
		{"a duration is not a status", "timed out after 500ms", 0, false},
		{"empty", "", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := OutcomeErrorStatus(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("OutcomeErrorStatus(%q) = %d,%v; want %d,%v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}
