// Package ratelimit holds the newest Claude subscription rate-limit snapshot
// this pod has observed. The statusline command (cmd/cc-statusline) POSTs each
// snapshot to the loopback internal API, which Sets it here; the turn finaliser
// reads it and attaches it to the operator callback.
//
// The snapshot is ACCOUNT-WIDE, not per-turn: it describes the shared
// subscription every agent pod in the fleet burns, so it is deliberately NOT
// carried on turn.Record's per-turn accounting path.
package ratelimit

import (
	"sync"
	"time"
)

// Snapshot is one observation of the account's usage windows. Percents are
// 0..100 and may be fractional. A zero Reset means "unknown", which the
// operator's gate treats as an inactive window rather than a live one.
//
// The two windows are carried INDEPENDENTLY: claude reports five_hour and
// seven_day as individually optional, so a snapshot legitimately holds one and
// not the other.
type Snapshot struct {
	ObservedAt      time.Time
	FiveHourPercent float64
	FiveHourReset   time.Time
	WeeklyPercent   float64
	WeeklyReset     time.Time
}

// Latest is a concurrency-safe newest-wins holder. The zero value is empty and
// reports ok=false, which is what distinguishes "no snapshot yet" from a real
// "0% used": a pod whose statusline never fired attaches nothing to its
// callback and leaves the operator gate exactly as inert as before, rather
// than feeding it a fabricated zero.
type Latest struct {
	mu   sync.RWMutex
	snap Snapshot
	set  bool
}

// Set stores s when it is newer than what is held. Out-of-order arrivals are
// dropped rather than overwriting: the statusline fires on TUI redraws with no
// ordering guarantee across concurrent processes.
func (l *Latest) Set(s Snapshot) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.set && !s.ObservedAt.After(l.snap.ObservedAt) {
		return
	}
	l.snap = s
	l.set = true
}

// Get returns the held snapshot; ok is false until something has been Set.
func (l *Latest) Get() (Snapshot, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.snap, l.set
}
