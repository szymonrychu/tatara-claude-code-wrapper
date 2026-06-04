package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
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
