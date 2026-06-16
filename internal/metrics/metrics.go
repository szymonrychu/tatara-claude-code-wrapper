// Package metrics holds the wrapper's prometheus collectors.
package metrics

import "github.com/prometheus/client_golang/prometheus"

type Metrics struct {
	TurnsTotal        *prometheus.CounterVec
	TurnDuration      prometheus.Histogram
	TurnInFlight      prometheus.Gauge
	ClaudeRestarts    prometheus.Counter
	WebhookDelivery   *prometheus.CounterVec
	HookReceived      prometheus.Counter
	StreamEventsTotal *prometheus.CounterVec
	Interjections     prometheus.Counter

	// Bootstrap metrics (rule 13: counters for everything that counts/can fail).
	BootstrapCloneTotal  *prometheus.CounterVec // label: result=ok|fail
	BootstrapDuration    prometheus.Histogram   // full Render() wall-clock time
	CommitPushTotal      *prometheus.CounterVec // label: result=ok|fail
	BootstrapHookInstall *prometheus.CounterVec // labels: result=ok|fail, tool=mise|pre-commit
	HookOutcome          *prometheus.CounterVec // labels: result=ok|bad_payload|rejected|store_error
	MetricPushTotal      *prometheus.CounterVec // labels: result=ok|encode_fail|transport_fail|rejected
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
		StreamEventsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_stream_events_total", Help: "Transcript stream events emitted by stream_type."}, []string{"stream_type"}),
		Interjections: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ccw_interjections_total", Help: "Mid-turn interjections injected into the live session."}),
		BootstrapCloneTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_bootstrap_clone_total", Help: "Bootstrap repo clone/resume attempts by result."}, []string{"result"}),
		BootstrapDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "ccw_bootstrap_duration_seconds", Help: "Full Render() bootstrap wall-clock duration.",
			Buckets: prometheus.ExponentialBuckets(0.5, 2, 8)}),
		CommitPushTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_commit_push_total", Help: "CommitAndPush calls by result."}, []string{"result"}),
		BootstrapHookInstall: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_bootstrap_hook_install_total", Help: "Hook install attempts (mise/pre-commit) by result and tool."}, []string{"result", "tool"}),
		HookOutcome: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_hook_outcome_total", Help: "Stop-hook callback outcomes at every decision point."}, []string{"result"}),
		MetricPushTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_metric_push_total", Help: "Metric push attempts by result."}, []string{"result"}),
	}
	reg.MustRegister(m.TurnsTotal, m.TurnDuration, m.TurnInFlight,
		m.ClaudeRestarts, m.WebhookDelivery, m.HookReceived, m.StreamEventsTotal, m.Interjections,
		m.BootstrapCloneTotal, m.BootstrapDuration, m.CommitPushTotal,
		m.BootstrapHookInstall, m.HookOutcome, m.MetricPushTotal)
	return m
}
