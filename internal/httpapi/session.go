package httpapi

import (
	"net/http"
	"os"
)

func (a *API) getSession(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.ctl.Snapshot())
}

func (a *API) getTranscript(w http.ResponseWriter, _ *http.Request) {
	p := a.ctl.TranscriptPath()
	if p == "" {
		http.Error(w, "no transcript yet", http.StatusNotFound)
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		http.Error(w, "transcript unavailable", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	_, _ = w.Write(b)
}

func (a *API) deleteSession(w http.ResponseWriter, r *http.Request) {
	_ = a.ctl.Shutdown(r.Context())
	w.WriteHeader(http.StatusAccepted)
}
