package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/pushclient"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/webhook"
)

// TestShutdown_TerminalCountersReachTheReceiver drives the real teardown
// sequence against a real Pusher and asserts that a counter written DURING
// teardown leaves the pod.
//
// The write here is the genuine article, not a stand-in: an unanswered probe
// is closed out by sess.Shutdown -> probes.Finalize() ->
// ccw_probe_outcomes_total{outcome="never_delivered"}. With pusher.Shutdown
// running FIRST, as it did, that increment lands on a registry nobody is
// pushing any more and whose run the receiver has already been told to drop -
// unobservable by construction, which is why the alert reading it has never
// been able to fire.
func TestShutdown_TerminalCountersReachTheReceiver(t *testing.T) {
	var (
		mu        sync.Mutex
		posts     []string
		sawDelete bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		switch r.Method {
		case http.MethodPost:
			posts = append(posts, string(b))
		case http.MethodDelete:
			sawDelete = true
		}
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	mgr := session.New(session.Config{}, turn.NewStore(), m, testLogger(), time.Now, func() string { return "turn-1" })
	// A probe written to the PTY and never delivered: exactly the last probe of
	// a pod's life, the one sent to the turn that got the pod stopped.
	mgr.ProbeTrackerForTest().Register("probe-1", "still there?", time.Now())

	pusher := pushclient.New(pushclient.Config{
		URL:      srv.URL + "/internal/metrics/push",
		RunID:    "run-shutdown",
		Interval: time.Hour, // only the immediate push and the final push fire
	}, reg, testLogger())
	pusher.Start()
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(posts) >= 1
	}, time.Second, 5*time.Millisecond)

	a := &app{
		log:      testLogger(),
		sess:     mgr,
		sender:   webhook.New(webhook.Config{Retries: 1}, m, testLogger()),
		pusher:   pusher,
		pub:      &http.Server{ReadHeaderTimeout: time.Second},
		internal: &http.Server{ReadHeaderTimeout: time.Second},
	}
	require.NoError(t, a.shutdown(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	require.False(t, sawDelete, "teardown must not DELETE the run before Prometheus can scrape it")
	require.GreaterOrEqual(t, len(posts), 2, "teardown must end with a final push")
	last := posts[len(posts)-1]
	require.True(t,
		strings.Contains(last, `ccw_probe_outcomes_total{outcome="never_delivered"} 1`),
		"the final push must carry probes.Finalize()'s outcome, got:\n%s", last)
}
