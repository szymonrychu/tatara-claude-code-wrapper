package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/auth"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/auth/testjwks"
)

func TestVerifier_ValidToken(t *testing.T) {
	srv := testjwks.NewServer(t)
	ctx := context.Background()

	v, err := auth.NewVerifier(ctx, auth.Config{
		Issuer:   srv.Issuer(),
		Audience: "tatara-memory",
	})
	require.NoError(t, err)

	tok := srv.SignTypedToken(t, testjwks.Claims{
		Issuer:   srv.Issuer(),
		Audience: []string{"tatara-memory"},
		Subject:  "user-1",
		Extra:    map[string]any{"preferred_username": "szymon"},
	})

	claims, err := v.Verify(ctx, tok)
	require.NoError(t, err)
	require.Equal(t, "user-1", claims.Subject)
	require.Equal(t, "szymon", claims.PreferredUsername)
}

func TestVerifier_ExpiredToken(t *testing.T) {
	srv := testjwks.NewServer(t)
	ctx := context.Background()

	v, err := auth.NewVerifier(ctx, auth.Config{
		Issuer:   srv.Issuer(),
		Audience: "tatara-memory",
	})
	require.NoError(t, err)

	tok := srv.SignTypedToken(t, testjwks.Claims{
		Issuer:    srv.Issuer(),
		Audience:  []string{"tatara-memory"},
		Subject:   "user-1",
		IssuedAt:  time.Now().Add(-2 * time.Hour),
		NotBefore: time.Now().Add(-2 * time.Hour),
		ExpiresAt: time.Now().Add(-time.Hour),
	})

	_, err = v.Verify(ctx, tok)
	require.Error(t, err)
	require.Contains(t, err.Error(), "expired")
}

func TestVerifier_WrongIssuer(t *testing.T) {
	srv := testjwks.NewServer(t)
	ctx := context.Background()

	v, err := auth.NewVerifier(ctx, auth.Config{
		Issuer:   srv.Issuer(),
		Audience: "tatara-memory",
	})
	require.NoError(t, err)

	tok := srv.SignTypedToken(t, testjwks.Claims{
		Issuer:   "https://evil.example/realms/master",
		Audience: []string{"tatara-memory"},
		Subject:  "user-1",
	})

	_, err = v.Verify(ctx, tok)
	require.Error(t, err)
}

func TestVerifier_WrongAudience(t *testing.T) {
	srv := testjwks.NewServer(t)
	ctx := context.Background()

	v, err := auth.NewVerifier(ctx, auth.Config{
		Issuer:   srv.Issuer(),
		Audience: "tatara-memory",
	})
	require.NoError(t, err)

	tok := srv.SignTypedToken(t, testjwks.Claims{
		Issuer:   srv.Issuer(),
		Audience: []string{"some-other-app"},
		Subject:  "user-1",
	})

	_, err = v.Verify(ctx, tok)
	require.Error(t, err)
}

func TestVerifier_BadSignature(t *testing.T) {
	srv := testjwks.NewServer(t)
	ctx := context.Background()

	v, err := auth.NewVerifier(ctx, auth.Config{
		Issuer:   srv.Issuer(),
		Audience: "tatara-memory",
	})
	require.NoError(t, err)

	foreign, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	tok := srv.SignTokenWithKey(t, foreign, testjwks.Claims{
		Issuer:   srv.Issuer(),
		Audience: []string{"tatara-memory"},
		Subject:  "user-1",
	})

	_, err = v.Verify(ctx, tok)
	require.Error(t, err)
}

func TestVerifier_MissingSubClaim(t *testing.T) {
	srv := testjwks.NewServer(t)
	ctx := context.Background()

	v, err := auth.NewVerifier(ctx, auth.Config{
		Issuer:   srv.Issuer(),
		Audience: "tatara-memory",
	})
	require.NoError(t, err)

	tok := srv.SignTypedToken(t, testjwks.Claims{
		Issuer:   srv.Issuer(),
		Audience: []string{"tatara-memory"},
		// Subject deliberately empty
	})

	_, err = v.Verify(ctx, tok)
	require.Error(t, err)
	require.Contains(t, err.Error(), "sub")
}
