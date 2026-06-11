package metrics_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
)

func TestNew_RegistersAllCollectors(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	require.NotNil(t, m)

	m.TurnsTotal.WithLabelValues("complete").Inc()
	m.TurnDuration.Observe(1.2)
	m.TurnInFlight.Set(1)
	m.ClaudeRestarts.Inc()
	m.WebhookDelivery.WithLabelValues("ok").Inc()
	m.HookReceived.Inc()

	mfs, err := reg.Gather()
	require.NoError(t, err)
	names := map[string]bool{}
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	for _, want := range []string{
		"ccw_turns_total", "ccw_turn_duration_seconds", "ccw_turn_in_flight",
		"ccw_claude_restarts_total", "ccw_webhook_delivery_total", "ccw_hook_received_total",
	} {
		require.True(t, names[want], "missing %s", want)
	}
}

func TestMetrics_StreamEventsTotal(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	m.StreamEventsTotal.WithLabelValues("text").Inc()
	m.StreamEventsTotal.WithLabelValues("tool_use").Inc()
	m.StreamEventsTotal.WithLabelValues("tool_use").Inc()

	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() == "ccw_stream_events_total" {
			vals := map[string]float64{}
			for _, metric := range mf.GetMetric() {
				for _, lp := range metric.GetLabel() {
					if lp.GetName() == "stream_type" {
						vals[lp.GetValue()] = metric.GetCounter().GetValue()
					}
				}
			}
			require.Equal(t, float64(1), vals["text"], "text count")
			require.Equal(t, float64(2), vals["tool_use"], "tool_use count")
			return
		}
	}
	t.Fatal("ccw_stream_events_total not found")
}
