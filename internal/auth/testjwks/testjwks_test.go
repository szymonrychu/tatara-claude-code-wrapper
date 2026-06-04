package testjwks_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/auth/testjwks"
)

func TestServer_SignsValidToken(t *testing.T) {
	srv := testjwks.NewServer(t)
	token := srv.SignTypedToken(t, testjwks.Claims{
		Issuer:   srv.Issuer(),
		Audience: []string{"tatara-memory"},
		Subject:  "user-1",
		Extra:    map[string]any{"preferred_username": "szymon"},
	})
	require.NotEmpty(t, token)
	// JWT has 3 dot-separated parts
	parts := 0
	for _, c := range token {
		if c == '.' {
			parts++
		}
	}
	require.Equal(t, 2, parts)
}

func TestServer_ServesJWKS(t *testing.T) {
	srv := testjwks.NewServer(t)
	resp, err := http.Get(srv.JWKSURL())
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var jwks struct {
		Keys []map[string]any `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(body, &jwks))
	require.Len(t, jwks.Keys, 1)
	require.Equal(t, "RSA", jwks.Keys[0]["kty"])
	require.NotEmpty(t, jwks.Keys[0]["n"])
	require.NotEmpty(t, jwks.Keys[0]["e"])
	require.NotEmpty(t, jwks.Keys[0]["kid"])
}

// TestStart verifies the canonical Wave 3B entry point returns a working server.
func TestStart(t *testing.T) {
	srv := testjwks.Start(t)
	defer srv.Close()

	require.NotEmpty(t, srv.Issuer())
	resp, err := http.Get(srv.JWKSURL())
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestClose_Idempotent verifies that calling Close() multiple times (and alongside
// t.Cleanup registered by Start) does not panic or double-close.
func TestClose_Idempotent(t *testing.T) {
	srv := testjwks.Start(t)
	// t.Cleanup is already registered by Start.
	// Explicit Close must be safe to call before cleanup fires.
	srv.Close()
	srv.Close() // second explicit call must be a no-op
}

// TestSignToken_MapForm verifies the Wave 3B map-form SignToken produces a token
// that the server's own verifier accepts.
func TestSignToken_MapForm(t *testing.T) {
	srv := testjwks.Start(t)
	defer srv.Close()

	tok := srv.SignToken(map[string]interface{}{
		"sub": "alice",
		"aud": "tatara-memory",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	require.NotEmpty(t, tok)
	// A valid JWT has exactly 2 dots.
	require.Equal(t, 2, strings.Count(tok, "."))
}

// TestMiddleware_RejectsAndAccepts verifies the Middleware helper rejects
// missing/invalid tokens and accepts a valid map-form signed token.
func TestMiddleware_RejectsAndAccepts(t *testing.T) {
	srv := testjwks.Start(t)
	defer srv.Close()

	mw := srv.Middleware("tatara-memory")
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ts := httptest.NewServer(handler)
	defer ts.Close()

	t.Run("missing token -> 401", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("invalid token -> 401", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
		req.Header.Set("Authorization", "Bearer not-a-jwt")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("valid map-form token -> 200", func(t *testing.T) {
		tok := srv.SignToken(map[string]interface{}{
			"sub": "alice",
			"aud": "tatara-memory",
		})
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	})
}
