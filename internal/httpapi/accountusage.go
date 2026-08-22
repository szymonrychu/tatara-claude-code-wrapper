package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/ratelimit"
)

// accountUsageRequest is the loopback wire body cmd/cc-statusline POSTs. Its
// JSON tags must match cmd/cc-statusline's report struct exactly.
type accountUsageRequest struct {
	ObservedAt        time.Time `json:"observedAt"`
	FiveHourPercent   float64   `json:"fiveHourPercent"`
	FiveHourResetUnix int64     `json:"fiveHourResetUnix,omitempty"`
	WeeklyPercent     float64   `json:"weeklyPercent"`
	WeeklyResetUnix   int64     `json:"weeklyResetUnix,omitempty"`
}

// accountUsage records the newest Claude subscription usage snapshot the
// statusline observed. Like /internal/turn-complete it is loopback-only and
// unauthenticated: the trust boundary is the pod's own network namespace.
//
// observedAt is REQUIRED, not defaulted to now: it is the newest-wins ordering
// key all the way through to the operator's fleet store, and a body without it
// is a malformed report rather than a fresh one.
func (a *API) accountUsage(w http.ResponseWriter, r *http.Request) {
	var req accountUsageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.countStatusline("bad_payload")
		a.log.WarnContext(r.Context(), "account-usage: bad payload",
			"action", "statusline_post", "request_id", middleware.GetReqID(r.Context()), "err", err)
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if req.ObservedAt.IsZero() {
		a.countStatusline("bad_payload")
		http.Error(w, "observedAt is required", http.StatusBadRequest)
		return
	}
	if a.rl != nil {
		a.rl.Set(ratelimit.Snapshot{
			ObservedAt:      req.ObservedAt,
			FiveHourPercent: req.FiveHourPercent,
			FiveHourReset:   unixOrZero(req.FiveHourResetUnix),
			WeeklyPercent:   req.WeeklyPercent,
			WeeklyReset:     unixOrZero(req.WeeklyResetUnix),
		})
	}
	a.countStatusline("ok")
	w.WriteHeader(http.StatusNoContent)
}

// unixOrZero maps a 0 unix timestamp to the zero time: "unknown reset", which
// the operator's gate treats as an inactive window rather than a live one. It
// is what a report carrying only the other window leaves behind.
func unixOrZero(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

func (a *API) countStatusline(result string) {
	if a.m != nil {
		a.m.StatuslineReports.WithLabelValues(result).Inc()
	}
}
