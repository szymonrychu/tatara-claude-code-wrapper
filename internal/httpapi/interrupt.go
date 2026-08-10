package httpapi

import "net/http"

// postInterrupt writes a single ESC byte into the running agent's PTY,
// cancelling the turn in flight without killing the pod.
//
// It takes no body. There is exactly one thing to say to a wedged agent
// through this channel and no parameters to say it with; POST /v1/probe is the
// endpoint for asking questions.
//
// Status codes:
//
//	202 - the ESC was written; body carries the interrupted turnId ("" if idle).
//	      Accepted, not OK: the turn resolves asynchronously once the
//	      interruption marker appears in the transcript, so the operator must
//	      poll GET /v1/session for the session to go Ready.
//	503 - no live PTY to write to (booting, dead, shutting down), or the write
//	      failed. Retryable.
//
// Deliberately no 409 and no 404-on-idle: interrupting an already-idle session
// is a harmless no-op, and the operator sending one has, by definition, just
// lost a race it should not have to reason about.
func (a *API) postInterrupt(w http.ResponseWriter, _ *http.Request) {
	turnID, err := a.ctl.Interrupt()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"turnId": turnID})
}
