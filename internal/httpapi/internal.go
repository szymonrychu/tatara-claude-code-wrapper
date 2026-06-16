package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
)

func (a *API) turnComplete(w http.ResponseWriter, r *http.Request) {
	var res session.HookResult
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
		if a.m != nil {
			a.m.HookOutcome.WithLabelValues("bad_payload").Inc()
		}
		a.log.WarnContext(r.Context(), "turn-complete: bad payload",
			"action", "hook_post", "request_id", middleware.GetReqID(r.Context()), "err", err)
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if err := a.ctl.Complete(res); err != nil {
		// session.Complete already logs and increments HookOutcome for rejected/store_error;
		// log at the HTTP boundary for observability of the 409 status.
		a.log.WarnContext(r.Context(), "turn-complete: rejected",
			"action", "hook_post", "request_id", middleware.GetReqID(r.Context()), "err", err)
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
