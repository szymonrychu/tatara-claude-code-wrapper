package httpapi

import (
	"net/http"
	"os"
	"strconv"
)

// ptyTail bounds the GET /v1/pty response: a sane default and a hard cap that
// matches the ring buffer's capacity.
const (
	ptyTailDefault = 4096
	ptyTailMax     = 64 * 1024
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

// getPTY returns the de-ANSI'd tail of the PTY ring buffer for live boot/wedge
// troubleshooting. ?bytes=N bounds the output (default 4096, capped at 64 KiB).
func (a *API) getPTY(w http.ResponseWriter, r *http.Request) {
	n := ptyTailDefault
	if v := r.URL.Query().Get("bytes"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	if n > ptyTailMax {
		n = ptyTailMax
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(a.ctl.Tail(n)))
}

func (a *API) deleteSession(w http.ResponseWriter, r *http.Request) {
	_ = a.ctl.Shutdown(r.Context())
	w.WriteHeader(http.StatusAccepted)
}
