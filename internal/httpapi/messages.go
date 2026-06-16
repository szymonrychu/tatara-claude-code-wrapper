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
//   - use the https scheme
//   - not resolve to loopback, link-local, or private (RFC1918) addresses
func validateCallbackURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid callbackUrl: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("callbackUrl must use https scheme, got %q", u.Scheme)
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
	// Reject link-local (169.254.x.x, fe80::/10).
	if ip.IsLinkLocalUnicast() {
		return fmt.Errorf("callbackUrl host %q is not allowed (link-local)", host)
	}
	// Reject private RFC1918 ranges (10.x, 172.16-31.x, 192.168.x).
	private := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	for _, cidr := range private {
		_, block, _ := net.ParseCIDR(cidr)
		if block.Contains(ip) {
			return fmt.Errorf("callbackUrl host %q is not allowed (private range %s)", host, cidr)
		}
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
