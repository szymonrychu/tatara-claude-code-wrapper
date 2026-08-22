package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParsePayload(t *testing.T) {
	tests := []struct {
		name           string
		in             string
		wantOK         bool
		wantFive       float64
		wantFiveReset  int64
		wantWeekly     float64
		wantWeeklReset int64
	}{
		{
			name:           "both windows present",
			in:             `{"rate_limits":{"five_hour":{"used_percentage":41.5,"resets_at":1755864000},"seven_day":{"used_percentage":73,"resets_at":1756296000}}}`,
			wantOK:         true,
			wantFive:       41.5,
			wantFiveReset:  1755864000,
			wantWeekly:     73,
			wantWeeklReset: 1756296000,
		},
		{
			name:          "only five_hour present",
			in:            `{"rate_limits":{"five_hour":{"used_percentage":12.25,"resets_at":1755864000}}}`,
			wantOK:        true,
			wantFive:      12.25,
			wantFiveReset: 1755864000,
		},
		{
			name:           "only seven_day present",
			in:             `{"rate_limits":{"seven_day":{"used_percentage":88,"resets_at":1756296000}}}`,
			wantOK:         true,
			wantWeekly:     88,
			wantWeeklReset: 1756296000,
		},
		{
			name:          "a real zero percent is still reported",
			in:            `{"rate_limits":{"five_hour":{"used_percentage":0,"resets_at":1755864000}}}`,
			wantOK:        true,
			wantFiveReset: 1755864000,
		},
		{name: "rate_limits absent before the first api response", in: `{"model":{"display_name":"Opus"}}`, wantOK: false},
		{name: "rate_limits explicitly null", in: `{"rate_limits":null}`, wantOK: false},
		{name: "rate_limits present but carries no window", in: `{"rate_limits":{}}`, wantOK: false},
		{name: "not json", in: `not json`, wantOK: false},
		{name: "empty", in: ``, wantOK: false},
		{
			name:   "rfc3339 resets_at is rejected rather than coerced to zero",
			in:     `{"rate_limits":{"five_hour":{"used_percentage":10,"resets_at":"2026-08-22T12:00:00Z"}}}`,
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parsePayload([]byte(tc.in), time.Now)
			require.Equal(t, tc.wantOK, ok)
			if !tc.wantOK {
				return
			}
			require.InDelta(t, tc.wantFive, got.FiveHourPercent, 0.001)
			require.Equal(t, tc.wantFiveReset, got.FiveHourResetUnix)
			require.InDelta(t, tc.wantWeekly, got.WeeklyPercent, 0.001)
			require.Equal(t, tc.wantWeeklReset, got.WeeklyResetUnix)
			require.False(t, got.ObservedAt.IsZero())
		})
	}
}

func TestRunWritesNothingToStdout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	var stdout strings.Builder
	body := `{"rate_limits":{"five_hour":{"used_percentage":5,"resets_at":1755864000},"seven_day":{"used_percentage":6,"resets_at":1756296000}}}`
	err := run(strings.NewReader(body), &stdout, srv.URL, time.Now)
	require.NoError(t, err)
	require.Empty(t, stdout.String(), "statusline stdout IS the rendered line and must stay empty")
}

// Nothing real to report means nothing posted. Posting a zero-valued snapshot
// would read to the operator's admission gate as "0% used", which is precisely
// the silent-inert failure this feed exists to fix.
func TestRunPostsNothingWithoutRateLimits(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "rate_limits absent", body: `{"model":{"display_name":"Opus"}}`},
		{name: "rate_limits null", body: `{"rate_limits":null}`},
		{name: "rate_limits carries no window", body: `{"rate_limits":{}}`},
		{name: "unparseable", body: `not json`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var posts atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				posts.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			var stdout strings.Builder
			require.NoError(t, run(strings.NewReader(tc.body), &stdout, srv.URL, time.Now))
			require.Zero(t, posts.Load(), "a snapshot-free payload must post nothing at all")
			require.Empty(t, stdout.String())
		})
	}
}

func TestRunSurvivesDeadEndpoint(t *testing.T) {
	var stdout strings.Builder
	body := `{"rate_limits":{"five_hour":{"used_percentage":5,"resets_at":1755864000},"seven_day":{"used_percentage":6,"resets_at":1756296000}}}`
	err := run(strings.NewReader(body), &stdout, "http://127.0.0.1:1/nope", time.Now)
	require.Error(t, err) // reported to the caller, which still exits 0
	require.Empty(t, stdout.String())
}
