package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/auth"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/httpapi"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/obs"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/pushclient"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/transcript"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/webhook"
)

// maxOutcomeReprompts bounds how many times this pod re-prompts the agent after
// the operator rejects a critical-outcome tool call. Beyond this the finaliser
// delivers the callback normally and the operator's empty-retry cap takes over.
const maxOutcomeReprompts = 2

type app struct {
	log      *slog.Logger
	pub      *http.Server
	internal *http.Server
	sess     *session.Manager
	sender   *webhook.Sender
	pusher   *pushclient.Pusher
	// safety is the mid-turn push-only safety net: it pushes the task branch to
	// origin on an interval so committed work is durable well before the turn
	// finaliser runs. See safetypush.go.
	safety *safetyPusher
	// turnWG tracks the per-turn finalisation goroutines (commit/push then
	// callback) spawned by OnTurnDone so shutdown can drain them and never lose
	// the agent's commits to a pod teardown mid-push.
	turnWG sync.WaitGroup
	// finishHook runs the conversationFinished lifecycle hook during shutdown,
	// bounded so a slow hook cannot stall teardown. Nil is a safe no-op.
	finishHook func(context.Context)
	// repromptMu guards outcomeReprompts, the per-pod count of times a rejected
	// critical-outcome MCP tool call (submit_outcome) was surfaced back to the
	// agent via a re-prompt instead of finishing the turn silently (Defect C).
	// Capped at maxOutcomeReprompts so a perpetually-failing agent still reaches
	// the operator's empty-retry cap rather than looping here.
	repromptMu       sync.Mutex
	outcomeReprompts int
	// submitFn is the turn-submit primitive reprompt() uses; production wires it
	// to a.sess.Submit. Injectable so the re-prompt budget logic is unit-testable
	// without a live PTY session.
	submitFn func(text, callbackURL string) (string, error)
	// heldInternalIssues carries report_internal_issue reports forward from a
	// turn whose callback the outcome re-prompt suppressed onto the next turn's
	// callback. That callback is a pod's only egress for these reports (agent
	// pods are not Loki-scraped), so without the hand-off a re-prompted turn's
	// reports are lost outright. Guarded by heldIssuesMu: finalizeTurn runs on a
	// per-turn goroutine.
	heldIssuesMu       sync.Mutex
	heldInternalIssues []turn.InternalIssueReport
}

// holdInternalIssues parks reports from a turn whose callback was suppressed by
// the outcome re-prompt, for the next turn's callback to carry.
func (a *app) holdInternalIssues(reports []turn.InternalIssueReport) {
	if len(reports) == 0 {
		return
	}
	a.heldIssuesMu.Lock()
	defer a.heldIssuesMu.Unlock()
	a.heldInternalIssues = append(a.heldInternalIssues, reports...)
}

// takePendingInternalIssues returns and clears the reports held from earlier
// suppressed turns.
func (a *app) takePendingInternalIssues() []turn.InternalIssueReport {
	a.heldIssuesMu.Lock()
	defer a.heldIssuesMu.Unlock()
	held := a.heldInternalIssues
	a.heldInternalIssues = nil
	return held
}

func newApp(ctx context.Context, cfg config) (*app, error) {
	log := obs.NewLogger(os.Stdout, parseLevel(cfg.LogLevel))
	reg := obs.PromRegistry()
	m := metrics.New(reg)

	if err := bootstrap.Render(buildBootstrapParams(cfg, log, m), gitRunner()); err != nil {
		return nil, err
	}
	setGitHubTokenEnv(cfg.GitToken)
	bootstrap.InstallHooks(cfg.Workspace, cfg.Repos, cfg.RepoURL, execRunnerDir(log), log, m)
	if err := tataraLookAndRegister(cfg.Workspace, execRunner(log)); err != nil {
		log.Error("tatara MCP registration failed; agent will have no operator tools", "error", err)
		return nil, err
	}

	// Primary repo the pod is bound to: first entry in cross-repo mode
	// (TATARA_REPOS), otherwise the single REPO_URL.
	repo := cfg.RepoURL
	if len(cfg.Repos) > 0 {
		repo = cfg.Repos[0].URL
	}

	store := turn.NewStore()
	sess := session.New(session.Config{
		ClaudePath:  cfg.ClaudePath,
		Workspace:   cfg.Workspace,
		HomeDir:     cfg.HomeDir,
		Env:         claudeEnv(cfg),
		Model:       cfg.Model,
		Effort:      cfg.Effort,
		Repo:        repo,
		Kind:        cfg.Kind,
		RepoName:    cfg.RepoName,
		Project:     cfg.Project,
		TurnTimeout: time.Duration(cfg.TurnTimeoutSeconds) * time.Second,
		PodTTL:      time.Duration(cfg.PodTTLSeconds) * time.Second,
		BootTimeout: time.Duration(cfg.BootTimeoutSeconds) * time.Second,
		SubmitSeq:   session.DefaultSubmitSeq,
	}, store, m, log, time.Now, newTurnID)

	sender := webhook.New(webhook.Config{Retries: cfg.WebhookRetries, Secret: cfg.CallbackHMACSecret}, m, log)
	defaultCB := cfg.DefaultCallbackURL

	a := &app{
		log:      log,
		sess:     sess,
		sender:   sender,
		pub:      &http.Server{Addr: cfg.HTTPAddr, ReadHeaderTimeout: 10 * time.Second},
		internal: &http.Server{Addr: cfg.InternalAddr, ReadHeaderTimeout: 10 * time.Second},
		finishHook: func(shutdownCtx context.Context) {
			fireLifecycleHookBounded(shutdownCtx, cfg, m, log, "conversationFinished",
				cfg.HookConversationFinished, 5*time.Second)
		},
	}
	// An outcome re-prompt is an ORDINARY turn (handoff=false): it is the pod
	// correcting its own tool call, not the operator's TTL handoff turn, so past
	// the pod deadline it is refused (ErrPodTTLExpired) and reprompt() falls back
	// to delivering the callback. It must never spend the one handoff slot.
	a.submitFn = func(text, callbackURL string) (string, error) {
		return a.sess.Submit(text, callbackURL, false)
	}

	sess.OnTurnDone = func(rec *turn.Record) {
		// Run the whole finalisation off the cc-stop-hook HTTP request goroutine:
		// OnTurnDone is invoked synchronously inside Complete, so a slow git push
		// here would block POST /internal/turn-complete past cc-stop-hook's 5s
		// per-attempt budget and trigger spurious retries. Tracked by turnWG so
		// shutdown drains it and the agent's commits are never lost to a pod
		// teardown that races the push.
		a.turnWG.Add(1)
		go func() {
			defer a.turnWG.Done()
			a.finalizeTurn(rec, cfg, m, log, sender, defaultCB)
		}()
	}

	// conversationRestart fires after each crash-relaunch that resumed the
	// conversation. Run in a goroutine so a slow hook cannot block the session's
	// watch/relaunch path. Set before Start so a relaunch during boot is covered.
	sess.OnRestart = func() {
		go fireLifecycleHook(cfg, m, log, "conversationRestart", cfg.HookConversationRestart, nil)
	}

	sess.StartTailer(ctx)

	if err := sess.Start(ctx); err != nil {
		return nil, err
	}
	// conversationStart fires once after the session boots successfully. Run
	// synchronously here (before serving traffic), matching the best-effort
	// boot-time semantics of InstallHooks.
	fireLifecycleHook(cfg, m, log, "conversationStart", cfg.HookConversationStart, nil)

	var verifier *auth.Verifier
	if cfg.OIDCIssuer != "" {
		v, err := auth.NewVerifier(ctx, auth.Config{Issuer: cfg.OIDCIssuer, Audience: cfg.OIDCAudience})
		if err != nil {
			return nil, err
		}
		verifier = v
	}

	api := httpapi.New(httpapi.Deps{Ctl: sess, Store: store, Verifier: verifier, Log: log, Registry: reg, Metrics: m})
	a.pub.Handler = api.Router()
	a.internal.Handler = api.InternalRouter()

	// Push-metrics client: this Pod is too short-lived to be reliably scraped,
	// so it pushes its /metrics to the operator's push-receiver. A no-op unless
	// the operator wired OPERATOR_PUSH_URL + RUN_ID.
	a.pusher = pushclient.New(pushclient.Config{
		URL:      cfg.OperatorPushURL,
		RunID:    cfg.RunID,
		Pod:      cfg.PodName,
		Interval: time.Duration(cfg.PushIntervalSeconds) * time.Second,
	}, reg, log, m)

	// Mid-turn git push safety net: the turn finaliser's commit+push only runs
	// at turn end, which on a 42-minute turn leaves committed work undurable for
	// most of the turn. A no-op unless this pod owns a task branch.
	a.safety = newSafetyPusher(cfg, gitRunner(), log, m)

	return a, nil
}

// finalizeTurn is the OnTurnDone finalisation logic, extracted from the
// closure that sets sess.OnTurnDone so it is directly testable: commits and
// pushes the agent's work, checks for a rejected critical-outcome tool call
// that warrants a re-prompt instead of a callback, drains the turn's
// report_internal_issue calls onto rec before delivery, delivers the callback,
// then fires the agentTurnFinished lifecycle hook. Called from a tracked
// background goroutine (never on the cc-stop-hook HTTP request goroutine).
func (a *app) finalizeTurn(rec *turn.Record, cfg config, m *metrics.Metrics, log *slog.Logger, sender *webhook.Sender, defaultCB string) {
	// Push BEFORE the callback: the operator's write-back reads the task
	// branch on receipt of the callback, so the branch must already carry
	// the agent's commits. A push failure is logged but must not drop the
	// callback (the operator still needs to learn the turn finished).
	if cfg.TaskBranch != "" {
		pushStart := time.Now()
		var err error
		if len(cfg.Repos) > 0 {
			var pushedRepos []string
			pushedRepos, err = bootstrap.CommitAndPushAll(cfg.Workspace, cfg.Repos, cfg.TaskBranch, "tatara agent: "+cfg.TaskBranch, gitRunner(), log, m)
			rec.PushedRepos = pushedRepos
		} else {
			// Single-repo clones into workspace/<owner>/<repo>, not the
			// workspace root, so commit/push must target that subdir.
			repoDir := bootstrap.RepoDir(cfg.Workspace, cfg.RepoURL)
			if repoDir == "" {
				err = fmt.Errorf("cannot derive repo dir from REPO_URL %q for commit/push", cfg.RepoURL)
			} else {
				var pushed bool
				pushed, err = bootstrap.CommitAndPush(repoDir, cfg.TaskBranch, "tatara agent: "+cfg.TaskBranch, gitRunner(), log, m)
				if pushed {
					rec.PushedRepos = []string{primaryRepoName(cfg)}
				}
			}
		}
		if err != nil {
			m.CommitPushTotal.WithLabelValues("fail").Inc()
			log.Error("commit/push failed", "action", "commit_push", "branch", cfg.TaskBranch, "error", err, "duration_ms", time.Since(pushStart).Milliseconds())
		} else {
			m.CommitPushTotal.WithLabelValues("ok").Inc()
			log.Info("commit/push succeeded", "action", "commit_push", "branch", cfg.TaskBranch,
				"pushed_repos", rec.PushedRepos, "duration_ms", time.Since(pushStart).Milliseconds())
		}
	}

	// Drain any report_internal_issue calls the agent made this turn so the
	// operator's callback carries them (agent pods are not Loki-scraped; only
	// the operator's collected stdout is alertable), and pick up anything a
	// previous re-prompted turn handed forward.
	//
	// This MUST stay above the outcome-reprompt block below, which returns
	// early to suppress this turn's callback. With the drain underneath it, a
	// re-prompted turn lost 100% of its reports: the accumulator is keyed by
	// turn id, so the next turn's drain never matches, and the suppressed
	// callback is the only egress a pod has (#136 pre-mortem 1, filed as a
	// residual in docs/superpowers/plans/2026-07-19-w1-lastcompleted-fallback.md).
	rec.InternalIssues = append(a.takePendingInternalIssues(), a.sess.DrainInternalIssues(rec.ID)...)

	// Defect C: a critical outcome tool (submit_outcome) the operator rejected
	// shows up in the turn transcript as an is_error tool_result. Rather than
	// let the turn finish silently (which the operator misreads as
	// "refused-no-explanation"), re-prompt the agent and skip THIS turn's
	// callback - the re-prompted turn delivers its own. Bounded by
	// maxOutcomeReprompts so a stuck agent still reaches the operator's cap.
	//
	// THE DIRECTIVE IS CLASSIFIED BY STATUS (tatara-operator#578). It used to be
	// one MANDATORY "call it again, do not finish the turn until it succeeds" for
	// EVERY is_error result. Against a 4xx that instruction is unsatisfiable by
	// construction - a client error means the identical call can never succeed -
	// so it turns one bad response into a turn loop and, through the operator's
	// pod-recreation budget, into a burned Task. Task
	// mt-i-mtg-decks-22-3f4594d8ce81d4d0 spent 7 pod runs and 3 pod recreations
	// that way on a single doomed 400. Only a 5xx or an unclassifiable transport
	// failure is retryable; see repromptDirective.
	if path := a.sess.TranscriptPath(); path != "" {
		toolName, errText, found, ferr := transcript.FailedCriticalOutcome(path)
		if ferr != nil {
			log.Warn("outcome-reprompt scan failed; delivering callback as-is",
				"action", "outcome_reprompt", "turn_id", rec.ID, "error", ferr)
		} else if found {
			retryable := retryableOutcomeFailure(errText)
			result := "reprompted"
			if !retryable {
				result = "reprompted_corrective"
			}
			if a.reprompt(toolName, errText, rec.CallbackURL) {
				m.OutcomeRepromptTotal.WithLabelValues(toolName, result).Inc()
				// Hand this turn's reports to the next one: rec is discarded
				// here and its callback never sent, so without this they are
				// lost outright.
				a.holdInternalIssues(rec.InternalIssues)
				log.Info("re-prompted agent after rejected outcome tool; suppressing this turn's callback",
					"action", "outcome_reprompt", "turn_id", rec.ID, "tool", toolName, "error", errText,
					"retryable", retryable, "held_internal_issues", len(rec.InternalIssues))
				return
			}
			m.OutcomeRepromptTotal.WithLabelValues(toolName, "budget_exhausted").Inc()
			log.Warn("outcome re-prompt budget exhausted; delivering callback so the operator cap applies",
				"action", "outcome_reprompt", "turn_id", rec.ID, "tool", toolName)
		}
	}

	url := rec.CallbackURL
	if url == "" {
		url = defaultCB
	}
	sender.DeliverPayload(url, rec.ID, newCallbackPayload(rec, cfg.TaskName))

	// agentTurnFinished runs last, after the turn's work is committed,
	// pushed, and the operator callback delivered. Best-effort.
	fireLifecycleHook(cfg, m, log, "agentTurnFinished", cfg.HookAgentTurnFinished,
		[]string{"TATARA_TURN_ID=" + rec.ID})
}

func (a *app) run() error {
	a.pusher.Start()
	a.safety.Start()
	errCh := make(chan error, 2)
	go func() { errCh <- a.internal.ListenAndServe() }()
	go func() { errCh <- a.pub.ListenAndServe() }()
	return <-errCh
}

func (a *app) shutdown(ctx context.Context) error {
	// Stop pushing and remove this run's series before the rest tears down, so
	// the operator drops them immediately rather than waiting for the TTL.
	a.pusher.Shutdown(ctx)
	// Stop the mid-turn safety net before the turn finalisers drain, so its
	// pushes never race the finaliser's own commit+push on the same branch.
	if a.safety != nil {
		a.safety.Shutdown(ctx)
	}
	_ = a.sess.Shutdown(ctx)
	// Drain the per-turn finalisation goroutines (commit/push then callback)
	// before tearing down the sender, so a push in flight is allowed to finish
	// and its callback is enqueued rather than dropped. Bounded by ctx via the
	// select below so a hung push cannot block shutdown indefinitely.
	turnsDone := make(chan struct{})
	go func() { a.turnWG.Wait(); close(turnsDone) }()
	select {
	case <-turnsDone:
	case <-ctx.Done():
		a.log.Warn("shutdown: turn finalisation goroutines did not drain in time", "err", ctx.Err())
	}
	// conversationFinished runs once the session is down and the turns have
	// drained, bounded so a slow hook cannot stall teardown.
	if a.finishHook != nil {
		a.finishHook(ctx)
	}
	// Drain in-flight webhook deliveries within a bounded window so retries
	// either complete or log a clean abort instead of being orphaned at exit.
	drainCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	a.sender.Shutdown(drainCtx)
	cancel()
	_ = a.internal.Shutdown(ctx)
	return a.pub.Shutdown(ctx)
}

// fireLifecycleHook runs a conversation/turn lifecycle hook best-effort in the
// workspace via the production hook runner. A no-op when command is empty.
func fireLifecycleHook(cfg config, m *metrics.Metrics, log *slog.Logger, name, command string, extraEnv []string) {
	bootstrap.RunHook(name, command, cfg.Workspace, nil, extraEnv, bootstrap.DefaultHookRunner, log, m)
}

// fireLifecycleHookBounded runs a lifecycle hook best-effort but never lets it
// block teardown beyond timeout (or past ctx cancellation). A no-op when the
// command is empty.
func fireLifecycleHookBounded(ctx context.Context, cfg config, m *metrics.Metrics, log *slog.Logger, name, command string, timeout time.Duration) {
	if command == "" {
		return
	}
	done := make(chan struct{})
	go func() {
		fireLifecycleHook(cfg, m, log, name, command, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		log.Warn("lifecycle hook did not finish before teardown deadline", "hook", name, "timeout", timeout)
	case <-ctx.Done():
		log.Warn("lifecycle hook aborted by shutdown context", "hook", name)
	}
}

func buildBootstrapParams(cfg config, log *slog.Logger, m *metrics.Metrics) bootstrap.Params {
	return bootstrap.Params{
		HomeDir:           cfg.HomeDir,
		Workspace:         cfg.Workspace,
		GlobalClaudeMd:    readFileOrEmpty(cfg.GlobalClaudeMdPath),
		ProjectClaudeMd:   readFileOrEmpty(cfg.ProjectClaudeMdPath),
		BaseMCP:           readBytesOrDefault(cfg.MCPBasePath, []byte(`{"mcpServers":{}}`)),
		MCPOverlayDir:     cfg.MCPOverlayDir,
		GrafanaMCPURL:     cfg.GrafanaMCPURL,
		SerenaMCPURL:      cfg.SerenaMCPURL,
		ExtraMCPServers:   cfg.ExtraMCPServers,
		ExtraSkillSources: cfg.ExtraSkillSources,
		SkillsSrc:         strings.Split(cfg.SkillsSrcDirs, ":"),
		SkillProfile:      cfg.SkillProfile,
		SkillsRepo:        cfg.SkillsRepo,
		SkillsRef:         cfg.SkillsRef,
		SkillsCloneDir:    skillsCloneDir(cfg.SkillsSrcDirs),
		AgentsSrc:         []string{filepath.Join(skillsCloneDir(cfg.SkillsSrcDirs), ".claude", "agents")},
		HookCommand:       cfg.HookPath,
		AllowedTools:      readLines(cfg.AllowedToolsPath),
		EnableAllMCP:      true,
		PermissionMode:    cfg.PermissionMode,
		Effort:            cfg.Effort,
		AnthropicAPIKey:   os.Getenv("ANTHROPIC_API_KEY"),
		RepoURL:           cfg.RepoURL,
		RepoBranch:        cfg.RepoBranch,
		GitToken:          cfg.GitToken,
		GitUserName:       cfg.GitUserName,
		GitUserEmail:      cfg.GitUserEmail,
		TaskBranch:        cfg.TaskBranch,
		CheckoutBranch:    cfg.CheckoutBranch,
		// Render pushes nothing; it needs the interval only to decide whether to
		// tell the agent the safety net is running (bootstrap.AutoPushEnabled).
		BranchPushInterval: time.Duration(cfg.BranchPushIntervalSeconds) * time.Second,
		FullClone:          cfg.FullClone,
		Repos:              cfg.Repos,

		HookPreClone:             cfg.HookPreClone,
		HookPostClone:            cfg.HookPostClone,
		HookConversationStart:    cfg.HookConversationStart,
		HookConversationRestart:  cfg.HookConversationRestart,
		HookAgentTurnFinished:    cfg.HookAgentTurnFinished,
		HookConversationFinished: cfg.HookConversationFinished,
		HookRun:                  bootstrap.DefaultHookRunner,

		Log: log,
		M:   m,
	}
}

// skillsCloneDir derives the skills-repo clone target from the first entry
// in the colon-separated SkillsSrcDirs string. With the default
// SKILLS_SRC_DIRS=/etc/wrapper/skills/skills this resolves to
// /etc/wrapper/skills, which is the directory the boot clone populates.
func skillsCloneDir(srcDirs string) string {
	for _, p := range strings.SplitN(srcDirs, ":", 2) {
		if p != "" {
			return filepath.Dir(p)
		}
	}
	return "/etc/wrapper/skills"
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func claudeEnv(cfg config) []string {
	env := []string{"TERM=xterm-256color"}
	for _, e := range os.Environ() {
		// Strip any ambient CLAUDE_CODE_SUBAGENT_MODEL: it forces every
		// subagent onto one model, silently overriding the typed agents'
		// baked model: frontmatter (explorer=haiku/tester=haiku,sonnet/
		// builder=sonnet/architect=opus - task-kind redesign Decision 6).
		// The wrapper itself never sets this; the strip is a
		// belt-and-suspenders guard in case an operator/chart change ever
		// adds it as a pod env var.
		if strings.HasPrefix(e, "CLAUDE_CODE_SUBAGENT_MODEL=") {
			continue
		}
		env = append(env, e)
	}
	if cfg.HomeDir != "" {
		env = append(env, "HOME="+cfg.HomeDir)
	}
	return env
}

// tataraLookAndRegister checks tatara is on PATH and wires its MCP server.
// Both the LookPath miss and the mcp-config failure are fatal: the agent cannot
// fulfil the operator contract (submit_outcome, scm_read, issue_write,
// mr_write) without this registration.
func tataraLookAndRegister(workspace string, run bootstrap.CmdRunner) error {
	if _, err := exec.LookPath("tatara"); err != nil {
		return fmt.Errorf("tatara not found on PATH; MCP tools unavailable: %w", err)
	}
	return bootstrap.RegisterTataraMCP(workspace, run)
}

func gitRunner() bootstrap.GitRunner {
	return func(dir string, args ...string) error {
		cmd := exec.Command("git", args...) //nolint:gosec // git is a fixed binary; args are operator-supplied config, not user input
		cmd.Dir = dir
		_, err := cmd.CombinedOutput()
		if err != nil {
			// Deliberately omit raw combined output: git stderr can contain
			// credential-helper expansions or remote URLs with tokens.
			return fmt.Errorf("git -C %s %v: %w", dir, args, err)
		}
		return nil
	}
}

func execRunner(log *slog.Logger) bootstrap.CmdRunner {
	return func(name string, args ...string) error {
		cmd := exec.Command(name, args...) //nolint:gosec // name+args are controlled by bootstrap (tatara mcp-config), not user input
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %v: %s: %w", name, args, string(out), err)
		}
		log.Info("mcp-config registered tatara server", "cmd", name, "args", args)
		return nil
	}
}

// setGitHubTokenEnv propagates the bot PAT to GITHUB_TOKEN and
// MISE_GITHUB_TOKEN so that mise (and the aqua backend) can make
// authenticated GitHub API calls during tool installs. Both keys carry a
// _TOKEN suffix and are therefore auto-redacted from logs by secretsFromEnv.
func setGitHubTokenEnv(token string) {
	if token == "" {
		return
	}
	os.Setenv("GITHUB_TOKEN", token)      //nolint:errcheck,gosec // os.Setenv only fails on invalid key
	os.Setenv("MISE_GITHUB_TOKEN", token) //nolint:errcheck,gosec
}

func execRunnerDir(log *slog.Logger) bootstrap.CmdRunnerDir {
	return func(dir, name string, args ...string) error {
		cmd := exec.Command(name, args...) //nolint:gosec // dir/name/args are controlled by bootstrap (mise/pre-commit), not user input
		cmd.Dir = dir
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %v in %s: %s: %w", name, args, dir, string(out), err)
		}
		log.Info("hook install succeeded", "cmd", name, "args", args, "dir", dir)
		return nil
	}
}

func readFileOrEmpty(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

func readBytesOrDefault(p string, def []byte) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		return def
	}
	return b
}

func readLines(p string) []string {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var out []string
	for _, ln := range strings.Split(string(b), "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func newTurnID() string { return "turn-" + strconv.FormatInt(time.Now().UnixNano(), 36) }

// retryableOutcomeFailure classifies a rejected outcome tool_result
// (tatara-operator#578). A 5xx is the operator failing on its own side and the
// identical call succeeds once it recovers; a 4xx is a CLIENT error and the
// identical call can never succeed.
//
// AN UNCLASSIFIABLE TEXT IS TREATED AS RETRYABLE. A transport failure, an MCP
// framing error or any wording that carries no status is exactly the case where
// a retry is the right move, and it must never be silently downgraded into "this
// is your fault, do not retry".
func retryableOutcomeFailure(errText string) bool {
	status, ok := transcript.OutcomeErrorStatus(errText)
	if !ok {
		return true
	}
	return status >= 500
}

// repromptDirective is the corrective text a re-prompt carries, and the whole
// point of splitting it in two (tatara-operator#578).
//
// THE MANDATORY FORM IS ONLY EVER LEGAL FOR A RETRYABLE FAILURE. "Do not finish
// the turn until the call succeeds" is satisfiable when the operator failed
// transiently. Aimed at a 4xx it is an instruction to loop forever on a call
// that is refused by construction, which is precisely what burned
// mt-i-mtg-decks-22: three identical re-submissions per turn, seven pod runs,
// three pod recreations, parked no-outcome.
//
// The non-retryable form keeps the reason this path exists - a MALFORMED payload
// is a 4xx the agent really can correct - while (a) forbidding an unchanged
// resend, and (b) explicitly authorising the agent to STOP and report, which is
// the only correct move against a precondition it cannot influence.
func repromptDirective(tool, errText string, retryable bool) string {
	errText = strings.TrimSpace(errText)
	if retryable {
		return fmt.Sprintf("Your %s call failed on the operator's side, which is not a rejection of "+
			"what you sent: %s. That is a transient failure, so the same call should succeed. "+
			"Call %s again with the SAME arguments. Do not finish the turn until the call succeeds.",
			tool, errText, tool)
	}
	return fmt.Sprintf("Your %s call was REFUSED by the operator: %s. This is a client error, so the "+
		"IDENTICAL call will never succeed - do not resend it unchanged and do not retry it in a loop. "+
		"If the refusal names something you can correct (a missing or malformed argument), fix that one "+
		"thing and call %s once more. If instead it describes a precondition you cannot change, STOP: "+
		"finish the turn and file it with report_internal_issue.",
		tool, errText, tool)
}

// reprompt submits a corrective turn telling the agent its critical outcome
// tool call was rejected, in the form repromptDirective picks for the failure's
// class. It returns true when a re-prompt was issued, false when the budget is
// exhausted or the session would not accept a new turn (in which case the caller
// delivers the callback so the operator's empty-retry cap applies). The
// corrective text reuses the same callback URL so the eventual completion still
// reaches the operator.
func (a *app) reprompt(tool, errText, callbackURL string) bool {
	a.repromptMu.Lock()
	if a.outcomeReprompts >= maxOutcomeReprompts {
		a.repromptMu.Unlock()
		return false
	}
	a.outcomeReprompts++
	a.repromptMu.Unlock()

	msg := repromptDirective(tool, errText, retryableOutcomeFailure(errText))
	if _, err := a.submitFn(msg, callbackURL); err != nil {
		// Roll back the budget consumption: no turn was actually submitted.
		a.repromptMu.Lock()
		a.outcomeReprompts--
		a.repromptMu.Unlock()
		a.log.Warn("outcome re-prompt submit failed; delivering callback instead",
			"action", "outcome_reprompt", "tool", tool, "error", err)
		return false
	}
	return true
}

// primaryRepoName is the human-facing name of the single repo a non-cross-repo
// pod is bound to, derived from the namespace path of REPO_URL ("owner/repo").
// Used to populate PushedRepos in single-repo mode where there is no RepoSpec.
func primaryRepoName(cfg config) string {
	if dir := bootstrap.RepoDir(cfg.Workspace, cfg.RepoURL); dir != "" {
		return filepath.Base(dir)
	}
	return cfg.RepoURL
}
