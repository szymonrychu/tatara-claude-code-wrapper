package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
)

func (a *API) turnComplete(w http.ResponseWriter, r *http.Request) {
	var res session.HookResult
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if err := a.ctl.Complete(res); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
