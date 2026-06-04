// Package metrics holds the wrapper's prometheus collectors.
package metrics

import "github.com/prometheus/client_golang/prometheus"

type Metrics struct {
	TurnsTotal      *prometheus.CounterVec
	TurnDuration    prometheus.Histogram
	TurnInFlight    prometheus.Gauge
	ClaudeRestarts  prometheus.Counter
	WebhookDelivery *prometheus.CounterVec
	HookReceived    prometheus.Counter
}

func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		TurnsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_turns_total", Help: "Turns by terminal result."}, []string{"result"}),
		TurnDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "ccw_turn_duration_seconds", Help: "Turn wall-clock duration.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 12)}),
		TurnInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ccw_turn_in_flight", Help: "Turns currently in flight (0 or 1)."}),
		ClaudeRestarts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ccw_claude_restarts_total", Help: "claude process restarts."}),
		WebhookDelivery: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_webhook_delivery_total", Help: "Webhook deliveries by result."}, []string{"result"}),
		HookReceived: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ccw_hook_received_total", Help: "Stop-hook callbacks received."}),
	}
	reg.MustRegister(m.TurnsTotal, m.TurnDuration, m.TurnInFlight,
		m.ClaudeRestarts, m.WebhookDelivery, m.HookReceived)
	return m
}
