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
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
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

// TestPushFiltersRuntimeFamilies verifies the push body carries only the
// wrapper-owned families (ccw_*, tatara_wrapper_*) and never the Go/process
// runtime collectors, which the operator drops as reserved_name (issue #59).
// The same registry still exposes go_*/process_* for the /metrics endpoint;
// only the push path narrows.
func TestPushFiltersRuntimeFamilies(t *testing.T) {
	cap := &capture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	ccw := prometheus.NewCounter(prometheus.CounterOpts{Name: "ccw_turns_total", Help: "h"})
	ccw.Inc()
	tw := prometheus.NewCounter(prometheus.CounterOpts{Name: "tatara_wrapper_internal_issue_total", Help: "h"})
	tw.Inc()
	reg.MustRegister(ccw, tw)

	p := pushclient.New(pushclient.Config{
		URL:      srv.URL + "/internal/metrics/push",
		RunID:    "run-filter",
		Interval: time.Hour,
	}, reg, discardLog())
	p.Start()
	require.Eventually(t, func() bool {
		cap.mu.Lock()
		defer cap.mu.Unlock()
		return cap.hits >= 1
	}, time.Second, 5*time.Millisecond)
	p.Shutdown(context.Background())

	cap.mu.Lock()
	defer cap.mu.Unlock()
	require.Contains(t, cap.body, "ccw_turns_total")
	require.Contains(t, cap.body, "tatara_wrapper_internal_issue_total")
	require.NotContains(t, cap.body, "go_", "Go runtime families must not be pushed")
	require.NotContains(t, cap.body, "process_", "process runtime families must not be pushed")

	// The registry itself (the /metrics endpoint's gatherer) still has them.
	mfs, err := reg.Gather()
	require.NoError(t, err)
	var names []string
	for _, mf := range mfs {
		names = append(names, mf.GetName())
	}
	require.Contains(t, names, "go_goroutines", "runtime collectors stay on the registry for /metrics")
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

// TestMetricPushTotal_OkIncremented verifies that a successful push increments
// ccw_metric_push_total{result=ok} (audit finding 7).
func TestMetricPushTotal_OkIncremented(t *testing.T) {
	cap := &capture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	appReg := prometheus.NewRegistry()
	appReg.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: "ccw_turns_total", Help: "h"}))

	pusherReg := prometheus.NewRegistry()
	m := metrics.New(pusherReg)

	p := pushclient.New(pushclient.Config{
		URL:      srv.URL + "/internal/metrics/push",
		RunID:    "run-ok",
		Interval: time.Hour,
	}, appReg, discardLog(), m)
	p.Start()
	require.Eventually(t, func() bool {
		cap.mu.Lock()
		defer cap.mu.Unlock()
		return cap.hits >= 1
	}, time.Second, 5*time.Millisecond)
	p.Shutdown(context.Background())

	mfs, err := pusherReg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() == "ccw_metric_push_total" {
			for _, metric := range mf.GetMetric() {
				for _, lp := range metric.GetLabel() {
					if lp.GetName() == "result" && lp.GetValue() == "ok" {
						require.GreaterOrEqual(t, metric.GetCounter().GetValue(), float64(1))
						return
					}
				}
			}
		}
	}
	t.Fatal("ccw_metric_push_total{result=ok} not found")
}

// TestMetricPushTotal_TransportFailIncremented verifies that a transport error
// increments ccw_metric_push_total{result=transport_fail} (audit finding 7).
func TestMetricPushTotal_TransportFailIncremented(t *testing.T) {
	appReg := prometheus.NewRegistry()
	appReg.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: "ccw_turns_total", Help: "h"}))

	pusherReg := prometheus.NewRegistry()
	m := metrics.New(pusherReg)

	// Use a closed server so the push fails with a transport error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // immediately close so transport fails

	p := pushclient.New(pushclient.Config{
		URL:      srv.URL + "/internal/metrics/push",
		RunID:    "run-fail",
		Interval: time.Hour,
	}, appReg, discardLog(), m)
	p.Start()
	require.Eventually(t, func() bool {
		mfs, _ := pusherReg.Gather()
		for _, mf := range mfs {
			if mf.GetName() == "ccw_metric_push_total" {
				for _, metric := range mf.GetMetric() {
					for _, lp := range metric.GetLabel() {
						if lp.GetName() == "result" && lp.GetValue() == "transport_fail" {
							return metric.GetCounter().GetValue() >= 1
						}
					}
				}
			}
		}
		return false
	}, time.Second, 5*time.Millisecond, "transport_fail not incremented")
	p.Shutdown(context.Background())
}

// TestPushHonorsPerPushDeadline verifies that a push goroutine never stalls
// beyond Interval: the loop context derived from pushWithTimeout is bounded by
// Interval so a stalled push returns promptly when the deadline fires (audit
// finding 2). A short Shutdown context ensures the delete call also returns fast.
func TestPushHonorsPerPushDeadline(t *testing.T) {
	// Server that hangs until the client disconnects.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	interval := 100 * time.Millisecond
	p := pushclient.New(pushclient.Config{
		URL:      srv.URL + "/push",
		RunID:    "r",
		Interval: interval,
	}, testRegistry(t), discardLog())
	p.Start()

	// Give the initial push time to start stalling, then shut down.
	time.Sleep(20 * time.Millisecond)

	// Shutdown with a short deadline so the delete call also cancels promptly.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), interval)
	defer shutCancel()

	done := make(chan struct{})
	go func() {
		p.Shutdown(shutCtx)
		close(done)
	}()
	select {
	case <-done:
		// success: Shutdown completed within the allotted time
	case <-time.After(3 * interval):
		t.Fatal("Shutdown blocked longer than 3x interval; per-push deadline not enforced")
	}
}

// TestEndpointAppendsAmpersandWhenURLContainsQuery verifies that endpoint()
// uses '&' when the base URL already contains a query string (audit finding 1).
func TestEndpointAppendsAmpersandWhenURLContainsQuery(t *testing.T) {
	cap := &capture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	// Provide a base URL that already has a query parameter.
	p := pushclient.New(pushclient.Config{
		URL:      srv.URL + "/push?existing=1",
		RunID:    "run-amp",
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
	// The full raw query should have existing=1 joined by & not ?
	require.Contains(t, cap.query, "existing=1")
	require.Contains(t, cap.query, "run_id=run-amp")
	// Ensure the separator is & (i.e. existing param is not duplicated as ?...?...)
	require.NotContains(t, cap.query, "?", "separator must be & not ?, got: "+cap.query)
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
