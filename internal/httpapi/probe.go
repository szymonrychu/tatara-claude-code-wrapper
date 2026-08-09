package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// postProbeReq is the body of POST /v1/probe. Text is optional: omitted, the
// wrapper uses session.DefaultProbeText, which already carries the exact
// TATARA-ALIVE instruction the answer detector keys on. A caller that supplies
// its own text is responsible for asking for that marker, or the probe can
// only ever reach "delivered".
type postProbeReq struct {
	Text string `json:"text,omitempty"`
}

// postProbe writes a question into the running agent's PTY mid-turn.
//
// It is a SEPARATE endpoint from POST /v1/messages rather than a flag on it
// because /v1/messages cannot do this and must not learn to: it maps
// session.ErrBusy to 409 for the entire duration of a turn, so the operator is
// locked out of the agent's context precisely while a turn is in flight. That
// is the only time a probe is worth sending. This handler therefore has no 409
// at all - a turn being in flight is the expected case, not an error.
//
// Status codes:
//
//	202 - written to the PTY; poll GET /v1/probe/{probeId} for the answer.
//	400 - malformed body.
//	503 - no live PTY to write to (booting, dead, shutting down), or the write
//	      failed. Retryable, unlike a 409.
func (a *API) postProbe(w http.ResponseWriter, r *http.Request) {
	var req postProbeReq
	// An empty body is a valid probe (use the default text), so only a
	// malformed body is rejected.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	id, err := a.ctl.Probe(req.Text)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	st, ok := a.ctl.ProbeStatus(id)
	if !ok {
		// The probe was superseded between the write and this read (a second
		// probe raced in). It was still sent, so this is a 202 carrying the id
		// alone rather than an error.
		writeJSON(w, http.StatusAccepted, map[string]string{"probeId": id})
		return
	}
	writeJSON(w, http.StatusAccepted, st)
}

// getProbe reports what the transcript has said about a probe so far. 404 means
// the id is unknown OR has been superseded by a later probe: only the most
// recent probe is retained, so there is no such thing as a stale-but-readable
// answer.
func (a *API) getProbe(w http.ResponseWriter, r *http.Request) {
	st, ok := a.ctl.ProbeStatus(chi.URLParam(r, "probeId"))
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
