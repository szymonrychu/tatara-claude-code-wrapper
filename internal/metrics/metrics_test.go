package metrics_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
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

// TestMetrics_InternalIssueTotal_Registered asserts the new wrapper metric is
// registered and carries the expected {category,severity} label pair.
func TestMetrics_InternalIssueTotal_Registered(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	require.NotNil(t, m.InternalIssueTotal)
	m.InternalIssueTotal.WithLabelValues("tool_error", "error").Inc()

	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() == "tatara_wrapper_internal_issue_total" {
			require.Len(t, mf.GetMetric(), 1)
			lbls := map[string]string{}
			for _, lp := range mf.GetMetric()[0].GetLabel() {
				lbls[lp.GetName()] = lp.GetValue()
			}
			require.Equal(t, "tool_error", lbls["category"])
			require.Equal(t, "error", lbls["severity"])
			require.Equal(t, float64(1), mf.GetMetric()[0].GetCounter().GetValue())
			return
		}
	}
	t.Fatal("tatara_wrapper_internal_issue_total not found")
}

// TestMetrics_InternalIssueDrainTimeoutTotal_Registered asserts the drain
// catch-up timeout counter is registered as a plain (unlabeled) counter.
func TestMetrics_InternalIssueDrainTimeoutTotal_Registered(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	require.NotNil(t, m.InternalIssueDrainTimeoutTotal)
	m.InternalIssueDrainTimeoutTotal.Inc()

	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() == "tatara_wrapper_internal_issue_drain_timeout_total" {
			require.Len(t, mf.GetMetric(), 1)
			require.Equal(t, float64(1), mf.GetMetric()[0].GetCounter().GetValue())
			return
		}
	}
	t.Fatal("tatara_wrapper_internal_issue_drain_timeout_total not found")
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

// TestMetrics_TokenCostLabels asserts the token+cost metrics carry the
// kind/repo/project labels sourced from the pod env (component 6). Table-driven
// over both families.
func TestMetrics_TokenCostLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)

	m.TurnTokensTotal.WithLabelValues("cache_read", "claude-sonnet-5", "review", "tatara-operator", "tatara").Add(42)
	m.TurnCostUSD.WithLabelValues("review", "tatara-operator", "tatara").Add(0.5)

	mfs, err := reg.Gather()
	require.NoError(t, err)
	byName := map[string]*dto.MetricFamily{}
	for _, mf := range mfs {
		byName[mf.GetName()] = mf
	}

	cases := []struct {
		metric     string
		wantLabels map[string]string
		wantValue  float64
	}{
		{
			metric:     "ccw_turn_tokens_total",
			wantLabels: map[string]string{"type": "cache_read", "model": "claude-sonnet-5", "kind": "review", "repo": "tatara-operator", "project": "tatara"},
			wantValue:  42,
		},
		{
			metric:     "ccw_turn_cost_usd_total",
			wantLabels: map[string]string{"kind": "review", "repo": "tatara-operator", "project": "tatara"},
			wantValue:  0.5,
		},
	}
	for _, tc := range cases {
		t.Run(tc.metric, func(t *testing.T) {
			mf := byName[tc.metric]
			require.NotNil(t, mf, "family %s not registered", tc.metric)
			require.Len(t, mf.GetMetric(), 1)
			got := map[string]string{}
			for _, lp := range mf.GetMetric()[0].GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			require.Equal(t, tc.wantLabels, got)
			require.Equal(t, tc.wantValue, mf.GetMetric()[0].GetCounter().GetValue())
		})
	}
}

// TestMetrics_TurnRefusals_Registered asserts the pod-TTL refusal counter is
// registered and carries a {reason} label: the fleet-wide view of how often
// pods are cut off mid-work, which is the signal that agentPodTTLSeconds is set
// too low (the per-refusal signal to the operator is the 410).
func TestMetrics_TurnRefusals_Registered(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	require.NotNil(t, m.TurnRefusals)
	m.TurnRefusals.WithLabelValues("pod_ttl_expired").Inc()

	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() == "ccw_turn_refusals_total" {
			require.Len(t, mf.GetMetric(), 1)
			lbls := map[string]string{}
			for _, lp := range mf.GetMetric()[0].GetLabel() {
				lbls[lp.GetName()] = lp.GetValue()
			}
			require.Equal(t, "pod_ttl_expired", lbls["reason"])
			require.Equal(t, float64(1), mf.GetMetric()[0].GetCounter().GetValue())
			return
		}
	}
	t.Fatal("ccw_turn_refusals_total not found")
}
