package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/auth"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/auth/testjwks"
)

func TestMiddleware_ValidTokenInjectsClaims(t *testing.T) {
	srv := testjwks.NewServer(t)
	ctx := context.Background()

	v, err := auth.NewVerifier(ctx, auth.Config{
		Issuer:   srv.Issuer(),
		Audience: "tatara-memory",
	})
	require.NoError(t, err)

	r := chi.NewRouter()
	r.Use(auth.Middleware(v))
	r.Get("/me", func(w http.ResponseWriter, req *http.Request) {
		c, ok := auth.ClaimsFromContext(req.Context())
		require.True(t, ok)
		_, _ = w.Write([]byte(c.Subject))
	})

	tok := srv.SignTypedToken(t, testjwks.Claims{
		Issuer:   srv.Issuer(),
		Audience: []string{"tatara-memory"},
		Subject:  "user-1",
	})

	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "user-1", rec.Body.String())
}

func TestMiddleware_MissingTokenReturns401(t *testing.T) {
	srv := testjwks.NewServer(t)
	v, err := auth.NewVerifier(context.Background(), auth.Config{
		Issuer: srv.Issuer(), Audience: "tatara-memory",
	})
	require.NoError(t, err)

	r := chi.NewRouter()
	r.Use(auth.Middleware(v))
	r.Get("/me", func(w http.ResponseWriter, _ *http.Request) {})

	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, `Bearer realm="tatara-memory"`, rec.Header().Get("WWW-Authenticate"))
}

func TestMiddleware_InvalidTokenReturns401(t *testing.T) {
	srv := testjwks.NewServer(t)
	v, err := auth.NewVerifier(context.Background(), auth.Config{
		Issuer: srv.Issuer(), Audience: "tatara-memory",
	})
	require.NoError(t, err)

	r := chi.NewRouter()
	r.Use(auth.Middleware(v))
	r.Get("/me", func(w http.ResponseWriter, _ *http.Request) {})

	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", "Bearer not-a-jwt")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, `Bearer realm="tatara-memory"`, rec.Header().Get("WWW-Authenticate"))
}

// TestMiddleware_AuthTotalCounterAndRequestIDInLog verifies that Middleware
// increments AuthTotal{result=rejected} on rejection and includes request_id and
// action=auth_reject in the structured WARN log (finding 6).
func TestMiddleware_AuthTotalCounterAndRequestIDInLog(t *testing.T) {
	srv := testjwks.NewServer(t)
	v, err := auth.NewVerifier(context.Background(), auth.Config{
		Issuer: srv.Issuer(), Audience: "tatara-memory",
	})
	require.NoError(t, err)

	reg := prometheus.NewRegistry()
	authTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ccw_auth_total", Help: "auth outcomes",
	}, []string{"result"})
	reg.MustRegister(authTotal)

	var logBuf bytes.Buffer
	handler := slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(slog.Default()) })

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(auth.Middleware(v, authTotal))
	r.Get("/me", func(w http.ResponseWriter, _ *http.Request) {})

	// Request with no token -> rejected.
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// Verify AuthTotal{result=rejected} incremented.
	mfs, err := reg.Gather()
	require.NoError(t, err)
	var rejectedVal float64
	for _, mf := range mfs {
		if mf.GetName() == "ccw_auth_total" {
			for _, mm := range mf.GetMetric() {
				for _, lp := range mm.GetLabel() {
					if lp.GetName() == "result" && lp.GetValue() == "rejected" {
						rejectedVal = mm.GetCounter().GetValue()
					}
				}
			}
		}
	}
	require.Equal(t, float64(1), rejectedVal, "AuthTotal{result=rejected} must be 1 on rejection")

	// Verify WARN log has action=auth_reject and request_id.
	data := logBuf.Bytes()
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	found := false
	for _, ln := range lines {
		var rec map[string]any
		if json.Unmarshal(ln, &rec) == nil && rec["level"] == "WARN" && rec["msg"] == "auth: rejected" {
			require.Equal(t, "auth_reject", rec["action"], "action must be auth_reject")
			require.NotNil(t, rec["request_id"], "request_id must be in WARN log")
			found = true
		}
	}
	require.True(t, found, "no WARN 'auth: rejected' log with action and request_id found")
}

// TestMiddleware_AuthTotalOkOnSuccess verifies AuthTotal{result=ok} on a valid token.
func TestMiddleware_AuthTotalOkOnSuccess(t *testing.T) {
	srv := testjwks.NewServer(t)
	ctx := context.Background()
	v, err := auth.NewVerifier(ctx, auth.Config{
		Issuer: srv.Issuer(), Audience: "tatara-memory",
	})
	require.NoError(t, err)

	reg := prometheus.NewRegistry()
	authTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ccw_auth_ok_total", Help: "auth outcomes ok",
	}, []string{"result"})
	reg.MustRegister(authTotal)

	r := chi.NewRouter()
	r.Use(auth.Middleware(v, authTotal))
	r.Get("/me", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	tok := srv.SignTypedToken(t, testjwks.Claims{
		Issuer:   srv.Issuer(),
		Audience: []string{"tatara-memory"},
		Subject:  "user-1",
	})
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	mfs, err := reg.Gather()
	require.NoError(t, err)
	var okVal float64
	for _, mf := range mfs {
		if mf.GetName() == "ccw_auth_ok_total" {
			for _, mm := range mf.GetMetric() {
				for _, lp := range mm.GetLabel() {
					if lp.GetName() == "result" && lp.GetValue() == "ok" {
						okVal = mm.GetCounter().GetValue()
					}
				}
			}
		}
	}
	require.Equal(t, float64(1), okVal, "AuthTotal{result=ok} must be 1 on success")
}
