package auth

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
)

type ctxKey struct{}

// ClaimsFromContext retrieves validated claims from the request context.
func ClaimsFromContext(ctx context.Context) (*Claims, bool) {
	c, ok := ctx.Value(ctxKey{}).(*Claims)
	return c, ok
}

const wwwAuthenticate = `Bearer realm="tatara-memory"`

// Middleware returns a chi-compatible middleware that verifies the Bearer token
// and injects parsed Claims into the request context. authTotal is optional;
// when non-nil it is incremented with result=ok|rejected.
func Middleware(v *Verifier, authTotal ...*prometheus.CounterVec) func(http.Handler) http.Handler {
	var ctr *prometheus.CounterVec
	if len(authTotal) > 0 {
		ctr = authTotal[0]
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqID := middleware.GetReqID(r.Context())
			raw, reason := bearerToken(r)
			if raw == "" {
				if ctr != nil {
					ctr.WithLabelValues("rejected").Inc()
				}
				slog.WarnContext(r.Context(), "auth: rejected",
					"action", "auth_reject", "request_id", reqID, "reason", reason)
				w.Header().Set("WWW-Authenticate", wwwAuthenticate)
				http.Error(w, "missing bearer token", http.StatusUnauthorized)
				return
			}
			claims, err := v.Verify(r.Context(), raw)
			if err != nil {
				if ctr != nil {
					ctr.WithLabelValues("rejected").Inc()
				}
				slog.WarnContext(r.Context(), "auth: rejected",
					"action", "auth_reject", "request_id", reqID, "reason", "invalid_token")
				w.Header().Set("WWW-Authenticate", wwwAuthenticate)
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}
			if ctr != nil {
				ctr.WithLabelValues("ok").Inc()
			}
			ctx := context.WithValue(r.Context(), ctxKey{}, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// bearerToken extracts the token from the Authorization header.
// Returns the token (empty on failure) and a rejection reason string.
func bearerToken(r *http.Request) (string, string) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", "missing_token"
	}
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", "invalid_scheme"
	}
	tok := strings.TrimSpace(h[len(prefix):])
	if tok == "" {
		return "", "missing_token"
	}
	return tok, ""
}
