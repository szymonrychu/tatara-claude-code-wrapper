package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
)

// validateCallbackURL rejects callbackUrl values that would enable SSRF.
// Empty string is allowed (caller uses server default). Non-empty values must:
//   - use the http or https scheme (in-cluster callbacks are plaintext http to
//     a ClusterIP svc with no TLS; the IP-range guards below, not the scheme,
//     are what provide the SSRF protection and fire for either scheme)
//   - not resolve to loopback, link-local, or private (RFC1918) addresses
func validateCallbackURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid callbackUrl: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("callbackUrl must use http or https scheme, got %q", u.Scheme)
	}
	host := u.Hostname()
	// Reject literal "localhost" before IP parsing.
	if host == "localhost" {
		return fmt.Errorf("callbackUrl host %q is not allowed (loopback)", host)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// hostname - allow (DNS resolution happens at delivery time; rejecting
		// all private hostnames by name is not feasible, but the scheme guard
		// and the IP guard together cover the metadata + direct-IP attack vectors)
		return nil
	}
	// Reject loopback (127.x.x.x, ::1).
	if ip.IsLoopback() {
		return fmt.Errorf("callbackUrl host %q is not allowed (loopback)", host)
	}
	// Reject unspecified (0.0.0.0, ::) which can route to localhost on some stacks.
	if ip.IsUnspecified() {
		return fmt.Errorf("callbackUrl host %q is not allowed (unspecified)", host)
	}
	// Reject link-local (169.254.x.x, fe80::/10) - covers the cloud metadata IP.
	if ip.IsLinkLocalUnicast() {
		return fmt.Errorf("callbackUrl host %q is not allowed (link-local)", host)
	}
	// Reject private ranges. net.IP.IsPrivate covers BOTH RFC1918 IPv4
	// (10/8, 172.16/12, 192.168/16) AND IPv6 unique-local fc00::/7, so an
	// internal IPv6 ULA target (e.g. [fd00::1]) cannot be reached either.
	if ip.IsPrivate() {
		return fmt.Errorf("callbackUrl host %q is not allowed (private range)", host)
	}
	return nil
}

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
	if err := validateCallbackURL(req.CallbackURL); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
