package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/ratelimit"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

func TestNewCallbackPayloadAccountUsage(t *testing.T) {
	rec := &turn.Record{ID: "t1", State: turn.Complete, StartedAt: time.Now()}
	t.Run("absent when nothing observed", func(t *testing.T) {
		p := newCallbackPayload(rec, "task-1", nil)
		require.Nil(t, p.AccountUsage)
		b, err := json.Marshal(p)
		require.NoError(t, err)
		require.NotContains(t, string(b), "accountUsage")
	})
	t.Run("carried when observed", func(t *testing.T) {
		p := newCallbackPayload(rec, "task-1", &accountUsagePayload{
			ObservedAt:      time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC),
			FiveHourPercent: 41.5,
			WeeklyPercent:   73,
		})
		b, err := json.Marshal(p)
		require.NoError(t, err)
		require.Contains(t, string(b), `"accountUsage"`)
		require.Contains(t, string(b), `"fiveHourPercent":41.5`)
		require.Contains(t, string(b), `"observedAt":"2026-08-22T10:00:00Z"`)
	})
}

// The reader is what keeps "no snapshot yet" out of the callback. A nil result
// is the SAFE value: the operator writes nothing and its gate stays exactly as
// inert as it is today, rather than reading a fabricated 0%.
func TestAppAccountUsage(t *testing.T) {
	t0 := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		holder     *ratelimit.Latest
		set        *ratelimit.Snapshot
		wantNil    bool
		wantFive   float64
		wantFiveTS int64
		wantWeekTS int64
	}{
		{name: "nil holder", holder: nil, wantNil: true},
		{name: "holder never set", holder: &ratelimit.Latest{}, wantNil: true},
		{
			name:   "a real zero percent is carried, not suppressed",
			holder: &ratelimit.Latest{},
			set: &ratelimit.Snapshot{
				ObservedAt: t0, FiveHourPercent: 0, FiveHourReset: time.Unix(1755864000, 0).UTC(),
			},
			wantFiveTS: 1755864000,
		},
		{
			name:   "weekly-only snapshot leaves the five-hour reset unset",
			holder: &ratelimit.Latest{},
			set: &ratelimit.Snapshot{
				ObservedAt: t0, WeeklyPercent: 73, WeeklyReset: time.Unix(1756296000, 0).UTC(),
			},
			wantWeekTS: 1756296000,
		},
		{
			name:   "both windows carried",
			holder: &ratelimit.Latest{},
			set: &ratelimit.Snapshot{
				ObservedAt: t0, FiveHourPercent: 41.5, FiveHourReset: time.Unix(1755864000, 0).UTC(),
				WeeklyPercent: 73, WeeklyReset: time.Unix(1756296000, 0).UTC(),
			},
			wantFive:   41.5,
			wantFiveTS: 1755864000,
			wantWeekTS: 1756296000,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := &app{rateLimits: tc.holder}
			if tc.set != nil {
				tc.holder.Set(*tc.set)
			}
			got := a.accountUsage()
			if tc.wantNil {
				require.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			require.Equal(t, t0, got.ObservedAt)
			require.InDelta(t, tc.wantFive, got.FiveHourPercent, 0.001)
			require.Equal(t, tc.wantFiveTS, got.FiveHourResetUnix)
			require.Equal(t, tc.wantWeekTS, got.WeeklyResetUnix)
		})
	}
}
