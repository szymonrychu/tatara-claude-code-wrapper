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
	LifecycleHookTotal   *prometheus.CounterVec // labels: result=ok|fail, hook=preClone|postClone|conversationStart|...
	HookOutcome          *prometheus.CounterVec // labels: result=ok|bad_payload|rejected|store_error
	MetricPushTotal      *prometheus.CounterVec // labels: result=ok|encode_fail|transport_fail|rejected

	// HTTP-layer metrics (rule 13: request count/latency/in-flight/panics).
	HTTPRequestsTotal   *prometheus.CounterVec   // labels: route, method, status_code
	HTTPRequestDuration *prometheus.HistogramVec // labels: route
	HTTPInFlight        prometheus.Gauge
	HTTPPanicsTotal     prometheus.Counter

	// Auth outcome metric (rule 13: everything that can fail).
	AuthTotal *prometheus.CounterVec // label: result=ok|rejected

	// Turn resume counter (rule 13: distinct fallible business action).
	TurnResumes *prometheus.CounterVec // labels: result=ok|write_fail, resume_mode=nudge|complete_from_transcript

	// Bootstrap render counter (rule 13: per-step observability).
	BootstrapRenderTotal *prometheus.CounterVec // label: result=ok|fail

	// Per-turn token/cost metrics (rule 13: tokens are the loop's primary cost
	// driver and the clearest runaway signal). Counters keep the cumulative-spend
	// property the operator can later budget on.
	TurnTokensTotal *prometheus.CounterVec // labels: type=input|output|cache_read|cache_creation, model
	TurnCostUSD     prometheus.Counter     // emitted only when result.json carries total_cost_usd

	// Conversation persistence (issue #114): upload-on-turn-finish and
	// restore-on-boot of the S3 transcript blob.
	ConversationOpsTotal *prometheus.CounterVec // labels: op=upload|restore, result=ok|fail|skip
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
		LifecycleHookTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_lifecycle_hook_total", Help: "Project lifecycle hook executions by result and hook name."}, []string{"result", "hook"}),
		HookOutcome: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_hook_outcome_total", Help: "Stop-hook callback outcomes at every decision point."}, []string{"result"}),
		MetricPushTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_metric_push_total", Help: "Metric push attempts by result."}, []string{"result"}),
		HTTPRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_http_requests_total", Help: "HTTP requests by route, method, and status code."}, []string{"route", "method", "status_code"}),
		HTTPRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "ccw_http_request_duration_seconds",
			Help:    "HTTP request latency by route.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route"}),
		HTTPInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ccw_http_in_flight", Help: "HTTP requests currently in flight."}),
		HTTPPanicsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ccw_http_panics_total", Help: "HTTP handler panics recovered."}),
		AuthTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_auth_total", Help: "Auth outcomes by result (ok|rejected)."}, []string{"result"}),
		TurnResumes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_turn_resumes_total", Help: "Turn resume attempts by result (ok|write_fail) and mode (nudge|complete_from_transcript)."}, []string{"result", "resume_mode"}),
		BootstrapRenderTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_bootstrap_render_total", Help: "Bootstrap config-render steps by result (ok|fail)."}, []string{"result"}),
		TurnTokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_turn_tokens_total", Help: "Claude tokens consumed per turn, summed across the turn, by token type and model."}, []string{"type", "model"}),
		TurnCostUSD: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ccw_turn_cost_usd_total", Help: "Cumulative Claude turn cost in USD (from result.json total_cost_usd when present)."}),
		ConversationOpsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_conversation_ops_total", Help: "Conversation transcript persistence operations by op and result."}, []string{"op", "result"}),
	}
	reg.MustRegister(m.TurnsTotal, m.TurnDuration, m.TurnInFlight,
		m.ClaudeRestarts, m.WebhookDelivery, m.HookReceived, m.StreamEventsTotal, m.Interjections,
		m.BootstrapCloneTotal, m.BootstrapDuration, m.CommitPushTotal,
		m.BootstrapHookInstall, m.LifecycleHookTotal, m.HookOutcome, m.MetricPushTotal,
		m.HTTPRequestsTotal, m.HTTPRequestDuration, m.HTTPInFlight, m.HTTPPanicsTotal,
		m.AuthTotal, m.TurnResumes, m.BootstrapRenderTotal,
		m.TurnTokensTotal, m.TurnCostUSD, m.ConversationOpsTotal)
	return m
}
