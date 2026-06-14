package metricspush_test

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
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metricspush"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func counterVal(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, c.Write(&m))
	return m.GetCounter().GetValue()
}

type recorded struct {
	method string
	path   string
	body   string
}

type capture struct {
	mu     sync.Mutex
	reqs   []recorded
	status int
}

func (c *capture) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.reqs = append(c.reqs, recorded{method: r.Method, path: r.URL.Path, body: string(b)})
		status := c.status
		c.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
	}
}

func (c *capture) snapshot() []recorded {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]recorded(nil), c.reqs...)
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

// newPusher returns a Pusher whose registry carries a known ccw_ series so the
// pushed body is assertable.
func newPusher(t *testing.T, cfg metricspush.Config) (*metricspush.Pusher, *metrics.Metrics) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	m.TurnsTotal.WithLabelValues("complete").Inc()
	if cfg.Job == "" {
		cfg.Job = "ccw"
	}
	if cfg.RunID == "" {
		cfg.RunID = "run-123"
	}
	if cfg.Pod == "" {
		cfg.Pod = "wrapper-abc"
	}
	return metricspush.New(cfg, reg, m, discardLogger()), m
}

func TestPush_PayloadHasLabelsAndSeries(t *testing.T) {
	cap := &capture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	p, _ := newPusher(t, metricspush.Config{
		URL: srv.URL, Interval: time.Hour, RunID: "run-xyz", Pod: "pod-1", Job: "ccw",
	})
	p.Shutdown(context.Background()) // final push then delete

	reqs := cap.snapshot()
	var put *recorded
	for i := range reqs {
		if reqs[i].method == http.MethodPut {
			put = &reqs[i]
			break
		}
	}
	require.NotNil(t, put, "expected a PUT push request")
	require.Equal(t, "/metrics/job/ccw/run_id/run-xyz/pod/pod-1", put.path)
	require.Contains(t, put.body, "ccw_turns_total")
}

func TestPush_Periodic(t *testing.T) {
	cap := &capture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	p, _ := newPusher(t, metricspush.Config{URL: srv.URL, Interval: 5 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	require.Eventually(t, func() bool { return cap.count() >= 2 }, time.Second, 2*time.Millisecond,
		"periodic push did not fire repeatedly")
}

func TestShutdown_FinalPushThenDelete(t *testing.T) {
	cap := &capture{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	// Interval far in the future so only the shutdown path produces requests.
	p, _ := newPusher(t, metricspush.Config{URL: srv.URL, Interval: time.Hour})
	p.Start(context.Background())
	p.Shutdown(context.Background())

	reqs := cap.snapshot()
	require.Len(t, reqs, 2)
	require.Equal(t, http.MethodPut, reqs[0].method, "final push")
	require.Equal(t, http.MethodDelete, reqs[1].method, "delete on graceful exit")
}

func TestPush_FailuresSwallowedAndCounted(t *testing.T) {
	cap := &capture{status: http.StatusInternalServerError}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	p, m := newPusher(t, metricspush.Config{URL: srv.URL, Interval: time.Hour})
	// Must not panic or block despite the server erroring on every request.
	done := make(chan struct{})
	go func() { p.Shutdown(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown blocked on failing server")
	}

	require.Equal(t, 1.0, counterVal(t, m.MetricsPush.WithLabelValues("push", "error")))
	require.Equal(t, 1.0, counterVal(t, m.MetricsPush.WithLabelValues("delete", "error")))
}

func TestPush_BlankURLIsNoop(t *testing.T) {
	p, _ := newPusher(t, metricspush.Config{URL: "", Interval: time.Millisecond})
	p.Start(context.Background())    // must not start a loop
	p.Shutdown(context.Background()) // must not panic
}

func TestPush_GenericNetworkFailureSwallowed(t *testing.T) {
	// Closed server: connection refused exercises the transport error path.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	p, m := newPusher(t, metricspush.Config{URL: url, Interval: time.Hour})
	p.Shutdown(context.Background())

	require.Equal(t, 1.0, counterVal(t, m.MetricsPush.WithLabelValues("push", "error")))
}
