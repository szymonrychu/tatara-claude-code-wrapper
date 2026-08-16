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

	// Bootstrap metrics (rule 13: counters for everything that counts/can fail).
	BootstrapCloneTotal *prometheus.CounterVec // label: result=ok|fail
	BootstrapDuration   prometheus.Histogram   // full Render() wall-clock time
	CommitPushTotal     *prometheus.CounterVec // label: result=ok|fail
	// SafetyPushTotal counts the mid-turn push-only safety net's ticks, per repo
	// pushed. Distinct from CommitPushTotal, which counts the turn finaliser:
	// this one never commits, so a rising fail here is a push problem alone.
	SafetyPushTotal      *prometheus.CounterVec // label: result=ok|fail
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
	TurnResumes *prometheus.CounterVec // labels: result=ok|write_fail, resume_mode=nudge|resubmit|complete_from_transcript

	// TurnRefusals counts turns the wrapper refused to start. The operator sees
	// the 410; this is the fleet-wide view of how often pods are being cut off
	// mid-work, which is the signal that agentPodTTLSeconds is set too low.
	TurnRefusals *prometheus.CounterVec // labels: reason

	// Outcome-rejection re-prompt counter (rule 13): a critical outcome tool
	// (submit_outcome) the operator rejected, surfaced back to the agent for
	// correction instead of finishing the turn silently.
	OutcomeRepromptTotal *prometheus.CounterVec // labels: tool, result=reprompted|budget_exhausted

	// Bootstrap render counter (rule 13: per-step observability).
	BootstrapRenderTotal *prometheus.CounterVec // label: result=ok|fail

	// Resumed-task-branch reconciliation against the repo's base branch
	// (issue #131). A resume that silently kept a stale tree is exactly the
	// business action rule 12/13 want visible, so every outcome is counted:
	// result=up_to_date|merged|conflict|fetch_fail|base_unresolved.
	BootstrapReconcileTotal *prometheus.CounterVec // label: result

	// Resumed-workspace repairs on a persistent /workspace volume. Both are
	// SILENT to the agent by design (nothing it can act on), so this counter is
	// the only fleet-wide view of how often volumes come back damaged.
	//
	// BootstrapLockCleared: stale .git locks a killed pod left holding. A rising
	// rate means pods are being killed mid-git, not that the cleanup is wrong.
	// The label is collapsed to the fixed file names plus "refs" so a branch
	// name can never blow up the cardinality.
	BootstrapLockCleared *prometheus.CounterVec // label: lock
	// BootstrapWorkspaceRecovered: every other repair, by kind (an aborted
	// merge/rebase, a rescue commit, a wrong-Task wipe, an unusable checkout).
	BootstrapWorkspaceRecovered *prometheus.CounterVec // label: kind

	// Oversized blobs the turn finaliser refused to stage (issue #131): a build
	// artifact swept up by `git add -A` poisons the task branch for every later
	// turn, and `--no-verify` bypasses the pre-commit size hook that would catch it.
	CommitOversizedBlobSkipped prometheus.Counter

	// Per-turn token/cost metrics (rule 13: tokens are the loop's primary cost
	// driver and the clearest runaway signal). Counters keep the cumulative-spend
	// property the operator can later budget on.
	TurnTokensTotal *prometheus.CounterVec // labels: type=input|output|cache_read|cache_creation, model, kind, repo, project
	TurnCostUSD     *prometheus.CounterVec // labels: kind, repo, project (from result.json total_cost_usd)

	// Internal issue reports from agents via the report_internal_issue MCP tool.
	InternalIssueTotal *prometheus.CounterVec // labels: category, severity

	// Internal-issue drain catch-up timeouts (degraded/best-effort path: the
	// tailer did not catch up before DrainInternalIssues drained anyway, so a
	// trailing report may have been dropped).
	InternalIssueDrainTimeoutTotal prometheus.Counter

	// Agent tool-call outcomes observed in the transcript (issue #51): a
	// tool_result with is_error=true is a mid-turn tool failure that otherwise
	// completes the turn "successfully" with zero fleet-visible signal. The tool
	// label is clamped to a bounded set (built-ins + tatara MCP surface, else
	// "other") so cardinality stays bounded (rule 13).
	ToolCallsTotal *prometheus.CounterVec // labels: tool, outcome=success|error

	// Queue operations observed in the transcript. Claude Code parks a message
	// that arrives while a turn is mid-tool-call and writes `enqueue`, then
	// `remove`/`dequeue` when it is actually delivered to the model. An enqueue
	// with no matching removal is the shape of a turn wedged inside one long
	// tool call, which silence alone cannot distinguish from productive work.
	QueueOpsTotal *prometheus.CounterVec // label: op=enqueue|remove|dequeue|other

	// Liveness probes. POST /v1/probe writes a question straight into the PTY
	// mid-turn, because POST /v1/messages 409s while a turn is in flight and is
	// therefore useless for asking a possibly-hung agent what it is doing.
	//
	// ProbesTotal counts the write attempt; ProbeOutcomesTotal counts what the
	// transcript said happened, and its three outcomes are three DIFFERENT
	// diagnoses, not one success and two flavours of timeout:
	// answered = alive and steerable; delivered_unanswered = reaching tool-call
	// boundaries but not responding; never_delivered = never reached a
	// boundary at all, i.e. wedged inside one long tool call.
	ProbesTotal        *prometheus.CounterVec // label: result=ok|write_fail|unavailable
	ProbeAnswerSeconds prometheus.Histogram   // send -> answer latency
	ProbeOutcomesTotal *prometheus.CounterVec // label: outcome=answered|delivered_unanswered|never_delivered

	// InterruptsTotal counts POST /v1/interrupt (a single ESC byte written to
	// the PTY). result=unconfirmed is the one worth alerting on: the ESC was
	// written but no interruption marker appeared in the transcript, which
	// means the turn is still in flight and the pod is still not free for the
	// operator's handoff turn.
	InterruptsTotal *prometheus.CounterVec // label: result=ok|write_fail|unavailable|unconfirmed

	// TurnStallSuspected counts turns that went a full TurnTimeout without any
	// transcript activity. It replaces the timeout FAILURE the wrapper used to
	// inflict at this point: the wrapper no longer kills turns on inactivity,
	// so this counter and Snapshot.StallSuspectedSince are the entire
	// wrapper-side output of the inactivity timer. It can fire repeatedly for
	// one turn - the timer re-arms - so it counts windows of silence, not
	// stalled turns.
	TurnStallSuspected prometheus.Counter

	// Skills delivery metrics (rule 13: boot-time clone and filter are fallible).
	// These carry the ccw_ prefix like every other wrapper family: agent pods
	// have no scrape target, and pushclient drops anything the push allowlist
	// does not admit, so a wrapper_*-named family never reaches Prometheus at
	// all (issue #173).
	SkillsInstalled     *prometheus.CounterVec // label: profile
	SkillsCloneFailures *prometheus.CounterVec // label: source (skills_repo|extra)
	AgentsInstalled     prometheus.Counter     // total typed-agent .md files installed at boot
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
		BootstrapCloneTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_bootstrap_clone_total", Help: "Bootstrap repo clone/resume attempts by result."}, []string{"result"}),
		BootstrapDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "ccw_bootstrap_duration_seconds", Help: "Full Render() bootstrap wall-clock duration.",
			Buckets: prometheus.ExponentialBuckets(0.5, 2, 8)}),
		CommitPushTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_commit_push_total", Help: "CommitAndPush calls by result."}, []string{"result"}),
		SafetyPushTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_safety_push_total", Help: "Mid-turn push-only safety-net pushes by result (ok|fail), counted per repo per tick."}, []string{"result"}),
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
			Name: "ccw_turn_resumes_total", Help: "Turn resume attempts by result (ok|write_fail) and mode (nudge|resubmit|complete_from_transcript)."}, []string{"result", "resume_mode"}),
		TurnRefusals: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_turn_refusals_total", Help: "Turns the wrapper refused to start, by reason (pod_ttl_expired)."}, []string{"reason"}),
		OutcomeRepromptTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_outcome_reprompt_total", Help: "Critical-outcome MCP tool rejections re-prompted to the agent, by tool and result."}, []string{"tool", "result"}),
		BootstrapRenderTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_bootstrap_render_total", Help: "Bootstrap config-render steps by result (ok|fail)."}, []string{"result"}),
		BootstrapReconcileTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_bootstrap_reconcile_total", Help: "Resumed task-branch reconciliations by result, against the repo base branch (up_to_date|merged|conflict|fetch_fail|base_unresolved) and against the branch's own remote counterpart (local_only|local_ahead|remote_ahead|local_remote_merged|local_remote_conflict)."}, []string{"result"}),
		BootstrapLockCleared: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_bootstrap_lock_cleared_total", Help: "Stale git lock files removed from an inherited workspace, by lock (index.lock|shallow.lock|HEAD.lock|config.lock|packed-refs.lock|refs)."}, []string{"lock"}),
		BootstrapWorkspaceRecovered: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_bootstrap_workspace_recovered_total", Help: "Repairs bootstrap made to a persistent workspace that outlived its pod, by kind."}, []string{"kind"}),
		CommitOversizedBlobSkipped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ccw_commit_oversized_blob_skipped_total", Help: "Oversized files the turn finaliser refused to stage, keeping build artifacts out of the task branch."}),
		TurnTokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_turn_tokens_total", Help: "Claude tokens consumed per turn, summed across the turn, by token type, model, Task kind, repo, and project."}, []string{"type", "model", "kind", "repo", "project"}),
		TurnCostUSD: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_turn_cost_usd_total", Help: "Cumulative Claude turn cost in USD (from result.json total_cost_usd when present), by Task kind, repo, and project."}, []string{"kind", "repo", "project"}),
		InternalIssueTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tatara_wrapper_internal_issue_total", Help: "Agent-reported internal issues observed by the wrapper tailer, by category and severity."}, []string{"category", "severity"}),
		InternalIssueDrainTimeoutTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tatara_wrapper_internal_issue_drain_timeout_total", Help: "Times DrainInternalIssues timed out waiting for the transcript tailer to catch up before draining anyway (degraded path, trailing report may be dropped)."}),
		ToolCallsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_tool_calls_total", Help: "Agent tool calls observed in the transcript by tool name (clamped) and outcome (success|error)."}, []string{"tool", "outcome"}),
		QueueOpsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_queue_operations_total", Help: "Queue operations observed in the transcript by operation (enqueue|remove|dequeue|other)."}, []string{"op"}),
		ProbesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_probes_total", Help: "Mid-turn liveness probes written to the PTY, by result (ok|write_fail|unavailable)."}, []string{"result"}),
		ProbeAnswerSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "ccw_probe_answer_seconds", Help: "Latency from writing a liveness probe to the agent answering it.",
			Buckets: prometheus.ExponentialBuckets(0.5, 2, 12)}),
		ProbeOutcomesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_probe_outcomes_total", Help: "Terminal liveness-probe outcomes (answered|delivered_unanswered|never_delivered); never_delivered means the turn never reached a tool-call boundary."}, []string{"outcome"}),
		InterruptsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_interrupts_total", Help: "ESC interrupts written to the PTY, by result (ok|write_fail|unavailable|unconfirmed); unconfirmed means no interruption marker reached the transcript and the turn is still in flight."}, []string{"result"}),
		TurnStallSuspected: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ccw_turn_stall_suspected_total", Help: "Windows of a full TurnTimeout with no transcript activity on an in-flight turn. The wrapper no longer fails such turns; the operator decides."}),
		SkillsInstalled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_skills_installed_total", Help: "Skills installed at boot by profile."}, []string{"profile"}),
		SkillsCloneFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ccw_skills_clone_failures_total", Help: "Skill-source boot-clone failures (after 3 retries) by source: skills_repo is the fleet-wide harness clone, extra is one TATARA_EXTRA_SKILL_SOURCES entry (a per-project config error)."}, []string{"source"}),
		AgentsInstalled: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ccw_agents_installed_total", Help: "Typed subagent .md files installed at boot."}),
	}
	reg.MustRegister(m.TurnsTotal, m.TurnDuration, m.TurnInFlight,
		m.ClaudeRestarts, m.WebhookDelivery, m.HookReceived, m.StreamEventsTotal,
		m.BootstrapCloneTotal, m.BootstrapDuration, m.CommitPushTotal, m.SafetyPushTotal,
		m.BootstrapHookInstall, m.LifecycleHookTotal, m.HookOutcome, m.MetricPushTotal,
		m.HTTPRequestsTotal, m.HTTPRequestDuration, m.HTTPInFlight, m.HTTPPanicsTotal,
		m.AuthTotal, m.TurnResumes, m.TurnRefusals, m.OutcomeRepromptTotal, m.BootstrapRenderTotal,
		m.BootstrapReconcileTotal, m.BootstrapLockCleared, m.BootstrapWorkspaceRecovered,
		m.CommitOversizedBlobSkipped,
		m.TurnTokensTotal, m.TurnCostUSD, m.InternalIssueTotal, m.InternalIssueDrainTimeoutTotal,
		m.ToolCallsTotal, m.QueueOpsTotal,
		m.ProbesTotal, m.ProbeAnswerSeconds, m.ProbeOutcomesTotal,
		m.InterruptsTotal, m.TurnStallSuspected,
		m.SkillsInstalled, m.SkillsCloneFailures, m.AgentsInstalled)
	return m
}
