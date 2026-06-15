package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPostWithRetry_RetriesOnTransientError verifies that a transient 503 is
// retried and the call ultimately succeeds on the second attempt.
func TestPostWithRetry_RetriesOnTransientError(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := postWithRetry(srv.URL, []byte(`{}`))
	require.NoError(t, err)
	require.Equal(t, 2, calls, "expected two attempts")
}

// TestPostWithRetry_ExhaustsRetries verifies that after maxAttempts consecutive
// failures the function returns an error.
func TestPostWithRetry_ExhaustsRetries(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := postWithRetry(srv.URL, []byte(`{}`))
	require.Error(t, err, "expected error after exhausting retries")
	require.Equal(t, maxPostAttempts, calls)
}

// TestPostWithRetry_StatusCodeChecked verifies that a non-2xx response is
// treated as failure (the original code returned nil regardless of status).
func TestPostWithRetry_StatusCodeChecked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	// 400 is not retryable - should still be returned as error after exhausting retries.
	err := postWithRetry(srv.URL, []byte(`{}`))
	require.Error(t, err)
}
