package pushclient_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/pushclient"
)

func testRegistry(t *testing.T) *prometheus.Registry {
	t.Helper()
	reg := prometheus.NewRegistry()
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "ccw_turns_total", Help: "h"})
	c.Inc()
	reg.MustRegister(c)
	return reg
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type capture struct {
	mu     sync.Mutex
	method string
	query  string
	ctype  string
	body   string
	hits   int
}

// handler records POST requests only; the shutdown DELETE is ignored so it
// cannot overwrite the captured push.
func (c *capture) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPost {
			c.mu.Lock()
			c.method = r.Method
			c.query = r.URL.RawQuery
			c.ctype = r.Header.Get("Content-Type")
			c.body = string(b)
			c.hits++
			c.mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func TestDisabledWhenUnconfigured(t *testing.T) {
	p := pushclient.New(pushclient.Config{}, testRegistry(t), discardLog())
	require.False(t, p.Enabled())
	// Start/Shutdown must be safe no-ops.
	p.Start()
	p.Shutdown(context.Background())

	p2 := pushclient.New(pushclient.Config{URL: "http://x/push"}, testRegistry(t), discardLog())
	require.False(t, p2.Enabled(), "URL without run_id stays disabled")
}

func TestPushPostsTextWithIdentity(t *testing.T) {
	cap := &capture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	p := pushclient.New(pushclient.Config{
		URL:      srv.URL + "/internal/metrics/push",
		RunID:    "run-1",
		Pod:      "pod-a",
		Interval: time.Hour, // only the immediate push fires within the test
	}, testRegistry(t), discardLog())
	require.True(t, p.Enabled())

	p.Start()
	require.Eventually(t, func() bool {
		cap.mu.Lock()
		defer cap.mu.Unlock()
		return cap.hits >= 1
	}, time.Second, 5*time.Millisecond)
	p.Shutdown(context.Background())

	cap.mu.Lock()
	defer cap.mu.Unlock()
	require.Contains(t, cap.query, "run_id=run-1")
	require.Contains(t, cap.query, "pod=pod-a")
	require.Contains(t, cap.query, "job=tatara-claude-code-wrapper")
	require.Contains(t, cap.ctype, "text/plain")
	require.Contains(t, cap.body, "ccw_turns_total")
}

func TestShutdownDeletesSeries(t *testing.T) {
	var (
		mu      sync.Mutex
		deleted bool
		delQ    string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if r.Method == http.MethodDelete {
			mu.Lock()
			deleted = true
			delQ = r.URL.RawQuery
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	p := pushclient.New(pushclient.Config{
		URL:      srv.URL + "/internal/metrics/push",
		RunID:    "run-9",
		Interval: time.Hour,
	}, testRegistry(t), discardLog())
	p.Start()
	p.Shutdown(context.Background())

	mu.Lock()
	defer mu.Unlock()
	require.True(t, deleted, "expected a DELETE on shutdown")
	require.Contains(t, delQ, "run_id=run-9")
}

// A custom job label overrides the default.
func TestJobOverride(t *testing.T) {
	cap := &capture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()
	p := pushclient.New(pushclient.Config{
		URL:      srv.URL + "/push",
		RunID:    "r",
		Job:      "custom",
		Interval: time.Hour,
	}, testRegistry(t), discardLog())
	p.Start()
	require.Eventually(t, func() bool {
		cap.mu.Lock()
		defer cap.mu.Unlock()
		return cap.hits >= 1
	}, time.Second, 5*time.Millisecond)
	p.Shutdown(context.Background())
	cap.mu.Lock()
	defer cap.mu.Unlock()
	require.Contains(t, cap.query, "job=custom")
	require.Equal(t, http.MethodPost, cap.method)
}
