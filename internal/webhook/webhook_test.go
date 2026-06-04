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
	s.Deliver(context.Background(), srv.URL, rec)
	require.Eventually(t, func() bool { return atomic.LoadInt32(&hits) == 2 }, time.Second, 5*time.Millisecond)
}

func TestDeliver_EmptyURLIsNoop(t *testing.T) {
	s := newSender(t, 1)
	s.Deliver(context.Background(), "", &turn.Record{ID: "t1"}) // must not panic
}
