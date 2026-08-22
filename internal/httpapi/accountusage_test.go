package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/ratelimit"
)

func TestAccountUsageRoute(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantStatus   int
		wantStored   bool
		wantFive     float64
		wantWeekly   float64
		wantFiveZero bool
	}{
		{
			name:       "valid report is stored",
			body:       `{"observedAt":"2026-08-22T10:00:00Z","fiveHourPercent":41.5,"fiveHourResetUnix":1755864000,"weeklyPercent":73,"weeklyResetUnix":1756296000}`,
			wantStatus: http.StatusNoContent,
			wantStored: true,
			wantFive:   41.5,
			wantWeekly: 73,
		},
		{
			name:         "weekly-only report is stored with an unknown five-hour reset",
			body:         `{"observedAt":"2026-08-22T10:00:00Z","weeklyPercent":88,"weeklyResetUnix":1756296000}`,
			wantStatus:   http.StatusNoContent,
			wantStored:   true,
			wantWeekly:   88,
			wantFiveZero: true,
		},
		{name: "bad json is rejected", body: `nope`, wantStatus: http.StatusBadRequest},
		{name: "missing observedAt is rejected", body: `{"fiveHourPercent":10}`, wantStatus: http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var latest ratelimit.Latest
			a := New(Deps{RateLimits: &latest})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/internal/account-usage", strings.NewReader(tc.body))
			a.InternalRouter().ServeHTTP(rec, req)
			require.Equal(t, tc.wantStatus, rec.Code)
			got, ok := latest.Get()
			require.Equal(t, tc.wantStored, ok)
			if !tc.wantStored {
				return
			}
			require.InDelta(t, tc.wantFive, got.FiveHourPercent, 0.001)
			require.InDelta(t, tc.wantWeekly, got.WeeklyPercent, 0.001)
			if tc.wantFiveZero {
				require.True(t, got.FiveHourReset.IsZero())
			} else {
				require.Equal(t, time.Unix(1755864000, 0).UTC(), got.FiveHourReset)
			}
		})
	}
}

func TestAccountUsageRouteNilHolder(t *testing.T) {
	a := New(Deps{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/account-usage",
		strings.NewReader(`{"observedAt":"2026-08-22T10:00:00Z","fiveHourPercent":1}`))
	a.InternalRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)
}
