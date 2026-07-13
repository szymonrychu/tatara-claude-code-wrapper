package httpapi_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/httpapi"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

// aliveCtl is a minimal SessionController stub for HTTP middleware tests.
type aliveCtl struct{}

func (a *aliveCtl) Submit(_, _ string, _ bool) (string, error) { return "t", nil }
func (a *aliveCtl) Complete(_ session.HookResult) error        { return nil }
func (a *aliveCtl) Snapshot() session.Snapshot                 { return session.Snapshot{State: session.Ready} }
func (a *aliveCtl) TranscriptPath() string                     { return "" }
func (a *aliveCtl) Alive() bool                                { return true }
func (a *aliveCtl) Shutdown(_ context.Context) error           { return nil }

func newAPIWithMetrics(t *testing.T) (*httpapi.API, *metrics.Metrics, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	api := httpapi.New(httpapi.Deps{
		Ctl:      &aliveCtl{},
		Store:    turn.NewStore(),
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Registry: reg,
		Metrics:  m,
	})
	return api, m, reg
}

// TestRouter_HTTPRequestsTotalRecorded verifies that the HTTP metrics middleware
// on Router() increments ccw_http_requests_total after a request (finding 2).
func TestRouter_HTTPRequestsTotalRecorded(t *testing.T) {
	api, _, reg := newAPIWithMetrics(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	mfs, err := reg.Gather()
	require.NoError(t, err)
	found := false
	for _, mf := range mfs {
		if mf.GetName() == "ccw_http_requests_total" {
			found = true
			require.Greater(t, len(mf.GetMetric()), 0, "ccw_http_requests_total must have at least one sample")
		}
	}
	require.True(t, found, "ccw_http_requests_total not recorded after request")
}

// TestRouter_HTTPRequestDurationObserved verifies the latency histogram is
// populated (finding 2).
func TestRouter_HTTPRequestDurationObserved(t *testing.T) {
	api, _, reg := newAPIWithMetrics(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	api.Router().ServeHTTP(httptest.NewRecorder(), req)

	mfs, err := reg.Gather()
	require.NoError(t, err)
	found := false
	for _, mf := range mfs {
		if mf.GetName() == "ccw_http_request_duration_seconds" {
			found = true
			require.Greater(t, mf.GetMetric()[0].GetHistogram().GetSampleCount(), uint64(0),
				"ccw_http_request_duration_seconds must have a sample")
		}
	}
	require.True(t, found, "ccw_http_request_duration_seconds not observed after request")
}

// TestRouter_HTTPInFlightZeroAfterRequest verifies the in-flight gauge returns
// to 0 after the request completes (finding 2).
func TestRouter_HTTPInFlightZeroAfterRequest(t *testing.T) {
	api, _, reg := newAPIWithMetrics(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	api.Router().ServeHTTP(httptest.NewRecorder(), req)

	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() == "ccw_http_in_flight" {
			require.Equal(t, float64(0), mf.GetMetric()[0].GetGauge().GetValue(),
				"ccw_http_in_flight must be 0 after request completes")
			return
		}
	}
	// Gauge with value 0 may not appear in output; that is acceptable (0 is the baseline).
}
