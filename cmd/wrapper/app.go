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
	// turnWG tracks the per-turn finalisation goroutines (commit/push then
	// callback) spawned by OnTurnDone so shutdown can drain them and never lose
	// the agent's commits to a pod teardown mid-push.
	turnWG sync.WaitGroup
	// finishHook runs the conversationFinished lifecycle hook during shutdown,
	// bounded so a slow hook cannot stall teardown. Nil is a safe no-op.
	finishHook func(context.Context)
	// repromptMu guards outcomeReprompts, the per-pod count of times a rejected
	// critical-outcome MCP tool call (decline_implementation/already_done) was
	// surfaced back to the agent via a re-prompt instead of finishing the turn
	// silently (Defect C). Capped at maxOutcomeReprompts so a perpetually-failing
	// agent still reaches the operator's empty-retry cap rather than looping here.
	repromptMu       sync.Mutex
	outcomeReprompts int
	// submitFn is the turn-submit primitive reprompt() uses; production wires it
	// to a.sess.Submit. Injectable so the re-prompt budget logic is unit-testable
	// without a live PTY session.
	submitFn func(text, callbackURL string) (string, error)
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
			pushedRepos, err = bootstrap.CommitAndPushAll(cfg.Workspace, cfg.Repos, cfg.TaskBranch, "tatara agent: "+cfg.TaskBranch, gitRunner())
			rec.PushedRepos = pushedRepos
		} else {
			// Single-repo clones into workspace/<owner>/<repo>, not the
			// workspace root, so commit/push must target that subdir.
			repoDir := bootstrap.RepoDir(cfg.Workspace, cfg.RepoURL)
			if repoDir == "" {
				err = fmt.Errorf("cannot derive repo dir from REPO_URL %q for commit/push", cfg.RepoURL)
			} else {
				var pushed bool
				pushed, err = bootstrap.CommitAndPush(repoDir, cfg.TaskBranch, "tatara agent: "+cfg.TaskBranch, gitRunner())
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

	// Defect C: a critical outcome tool (decline_implementation/already_done)
	// the operator rejected (e.g. blank reason -> 400) shows up in the turn
	// transcript as an is_error tool_result. Rather than let the turn finish
	// silently (which the operator misreads as "refused-no-explanation"),
	// re-prompt the agent to retry with a non-blank reason, and skip THIS
	// turn's callback - the re-prompted turn delivers its own. Bounded by
	// maxOutcomeReprompts so a stuck agent still reaches the operator's cap.
	if path := a.sess.TranscriptPath(); path != "" {
		toolName, errText, found, ferr := transcript.FailedCriticalOutcome(path)
		if ferr != nil {
			log.Warn("outcome-reprompt scan failed; delivering callback as-is",
				"action", "outcome_reprompt", "turn_id", rec.ID, "error", ferr)
		} else if found {
			if a.reprompt(toolName, errText, rec.CallbackURL) {
				m.OutcomeRepromptTotal.WithLabelValues(toolName, "reprompted").Inc()
				log.Info("re-prompted agent after rejected outcome tool; suppressing this turn's callback",
					"action", "outcome_reprompt", "turn_id", rec.ID, "tool", toolName, "error", errText)
				return
			}
			m.OutcomeRepromptTotal.WithLabelValues(toolName, "budget_exhausted").Inc()
			log.Warn("outcome re-prompt budget exhausted; delivering callback so the operator cap applies",
				"action", "outcome_reprompt", "turn_id", rec.ID, "tool", toolName)
		}
	}

	// Drain any report_internal_issue calls the agent made this turn so the
	// operator's callback carries them (agent pods are not Loki-scraped; only
	// the operator's collected stdout is alertable).
	rec.InternalIssues = a.sess.DrainInternalIssues(rec.ID)

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
	errCh := make(chan error, 2)
	go func() { errCh <- a.internal.ListenAndServe() }()
	go func() { errCh <- a.pub.ListenAndServe() }()
	return <-errCh
}

func (a *app) shutdown(ctx context.Context) error {
	// Stop pushing and remove this run's series before the rest tears down, so
	// the operator drops them immediately rather than waiting for the TTL.
	a.pusher.Shutdown(ctx)
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
		HomeDir:         cfg.HomeDir,
		Workspace:       cfg.Workspace,
		GlobalClaudeMd:  readFileOrEmpty(cfg.GlobalClaudeMdPath),
		ProjectClaudeMd: readFileOrEmpty(cfg.ProjectClaudeMdPath),
		BaseMCP:         readBytesOrDefault(cfg.MCPBasePath, []byte(`{"mcpServers":{}}`)),
		MCPOverlayDir:   cfg.MCPOverlayDir,
		GrafanaMCPURL:   cfg.GrafanaMCPURL,
		SerenaMCPURL:    cfg.SerenaMCPURL,
		SkillsSrc:       strings.Split(cfg.SkillsSrcDirs, ":"),
		SkillProfile:    cfg.SkillProfile,
		SkillsRepo:      cfg.SkillsRepo,
		SkillsRef:       cfg.SkillsRef,
		SkillsCloneDir:  skillsCloneDir(cfg.SkillsSrcDirs),
		AgentsSrc:       []string{filepath.Join(skillsCloneDir(cfg.SkillsSrcDirs), ".claude", "agents")},
		HookCommand:     cfg.HookPath,
		AllowedTools:    readLines(cfg.AllowedToolsPath),
		EnableAllMCP:    true,
		PermissionMode:  cfg.PermissionMode,
		Effort:          cfg.Effort,
		AnthropicAPIKey: os.Getenv("ANTHROPIC_API_KEY"),
		RepoURL:         cfg.RepoURL,
		RepoBranch:      cfg.RepoBranch,
		GitToken:        cfg.GitToken,
		GitUserName:     cfg.GitUserName,
		GitUserEmail:    cfg.GitUserEmail,
		TaskBranch:      cfg.TaskBranch,
		CheckoutBranch:  cfg.CheckoutBranch,
		FullClone:       cfg.FullClone,
		Repos:           cfg.Repos,

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

// reprompt submits a corrective turn telling the agent its critical outcome
// tool call was rejected and must be retried with a non-blank reason. It returns
// true when a re-prompt was issued, false when the budget is exhausted or the
// session would not accept a new turn (in which case the caller delivers the
// callback so the operator's empty-retry cap applies). The corrective text reuses
// the same callback URL so the eventual completion still reaches the operator.
func (a *app) reprompt(tool, errText, callbackURL string) bool {
	a.repromptMu.Lock()
	if a.outcomeReprompts >= maxOutcomeReprompts {
		a.repromptMu.Unlock()
		return false
	}
	a.outcomeReprompts++
	a.repromptMu.Unlock()

	msg := fmt.Sprintf("Your %s call was rejected by the operator: %s. "+
		"This is mandatory: call %s again with a clear, non-blank `reason` explaining the decision. "+
		"Do not finish the turn until the call succeeds.", tool, strings.TrimSpace(errText), tool)
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
