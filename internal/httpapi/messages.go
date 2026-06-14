package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
)

type postMessageReq struct {
	Text        string `json:"text"`
	CallbackURL string `json:"callbackUrl"`
}

func (a *API) postMessage(w http.ResponseWriter, r *http.Request) {
	var req postMessageReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}
	id, err := a.ctl.Submit(req.Text, req.CallbackURL)
	if errors.Is(err, session.ErrBusy) {
		http.Error(w, "session busy", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"turnId": id})
}

type postInterjectReq struct {
	Text string `json:"text"`
}

// postInterject injects new user input into the turn currently in flight. It
// returns 409 when no turn is running (the operator should Submit instead).
func (a *API) postInterject(w http.ResponseWriter, r *http.Request) {
	var req postInterjectReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}
	err := a.ctl.Interject(req.Text)
	if errors.Is(err, session.ErrNotBusy) {
		http.Error(w, "no in-flight turn", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{})
}

func (a *API) listMessages(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.store.List())
}

func (a *API) getMessage(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.store.Get(chi.URLParam(r, "turnID"))
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
