package ratelimit

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLatest(t *testing.T) {
	t0 := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		sets    []Snapshot
		wantOK  bool
		wantPct float64
	}{
		{name: "zero value reports nothing", wantOK: false},
		{
			name:    "single set is returned",
			sets:    []Snapshot{{ObservedAt: t0, FiveHourPercent: 41.5}},
			wantOK:  true,
			wantPct: 41.5,
		},
		{
			name: "newer set wins",
			sets: []Snapshot{
				{ObservedAt: t0, FiveHourPercent: 41.5},
				{ObservedAt: t0.Add(time.Minute), FiveHourPercent: 62},
			},
			wantOK:  true,
			wantPct: 62,
		},
		{
			name: "older set is ignored",
			sets: []Snapshot{
				{ObservedAt: t0.Add(time.Minute), FiveHourPercent: 62},
				{ObservedAt: t0, FiveHourPercent: 41.5},
			},
			wantOK:  true,
			wantPct: 62,
		},
		{
			name:    "a real zero percent is held and is distinguishable from no snapshot",
			sets:    []Snapshot{{ObservedAt: t0, FiveHourPercent: 0}},
			wantOK:  true,
			wantPct: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var l Latest
			for _, s := range tc.sets {
				l.Set(s)
			}
			got, ok := l.Get()
			require.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				require.InDelta(t, tc.wantPct, got.FiveHourPercent, 0.001)
			}
		})
	}
}

// The holder carries the two windows independently: a snapshot that saw only
// the weekly window must not be rejected for having no five-hour data.
func TestLatestCarriesWindowsIndependently(t *testing.T) {
	t0 := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	var l Latest
	l.Set(Snapshot{ObservedAt: t0, WeeklyPercent: 73, WeeklyReset: t0.Add(time.Hour)})
	got, ok := l.Get()
	require.True(t, ok)
	require.InDelta(t, 73, got.WeeklyPercent, 0.001)
	require.Zero(t, got.FiveHourPercent)
	require.True(t, got.FiveHourReset.IsZero())
}

func TestLatestConcurrent(t *testing.T) {
	var l Latest
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			l.Set(Snapshot{ObservedAt: time.Now(), FiveHourPercent: float64(i)})
		}
		close(done)
	}()
	for i := 0; i < 500; i++ {
		_, _ = l.Get()
	}
	<-done
}
