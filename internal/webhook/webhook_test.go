package webhook_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/webhook"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newSender(t *testing.T, retries int) *webhook.Sender {
	t.Helper()
	return webhook.New(webhook.Config{Retries: retries, Backoff: time.Millisecond},
		metrics.New(prometheus.NewRegistry()), discardLogger())
}

func TestDeliver_SucceedsAfterRetry(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := newSender(t, 3)
	rec := &turn.Record{ID: "t1", State: turn.Complete, FinalText: "PONG"}
	s.Deliver(srv.URL, rec)
	require.Eventually(t, func() bool { return atomic.LoadInt32(&hits) == 2 }, time.Second, 5*time.Millisecond)
}

func TestDeliver_EmptyURLIsNoop(t *testing.T) {
	s := newSender(t, 1)
	s.Deliver("", &turn.Record{ID: "t1"}) // must not panic
}

func TestShutdown_AbortsInFlightRetries(t *testing.T) {
	// Server always fails, so the retry loop would otherwise back off forever.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := webhook.New(webhook.Config{Retries: 1_000_000, Backoff: 50 * time.Millisecond},
		metrics.New(prometheus.NewRegistry()), discardLogger())
	s.Deliver(srv.URL, &turn.Record{ID: "t1", State: turn.Complete})

	// A drain window that elapses immediately must cancel the goroutine and join.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { s.Shutdown(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return; in-flight delivery was orphaned")
	}
}
