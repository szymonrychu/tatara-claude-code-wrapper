package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
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
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/webhook"
)

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
}

func newApp(ctx context.Context, cfg config) (*app, error) {
	log := obs.NewLogger(os.Stdout, parseLevel(cfg.LogLevel))
	reg := obs.PromRegistry()
	m := metrics.New(reg)

	if err := bootstrap.Render(buildBootstrapParams(cfg, log, m), gitRunner()); err != nil {
		return nil, err
	}
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
		Env:         claudeEnv(cfg),
		Model:       cfg.Model,
		Effort:      cfg.Effort,
		Repo:        repo,
		TurnTimeout: time.Duration(cfg.TurnTimeoutSeconds) * time.Second,
		BootTimeout: time.Duration(cfg.BootTimeoutSeconds) * time.Second,
		SubmitSeq:   session.DefaultSubmitSeq,
	}, store, m, log, time.Now, newTurnID)

	sender := webhook.New(webhook.Config{Retries: cfg.WebhookRetries}, m, log)
	defaultCB := cfg.DefaultCallbackURL

	a := &app{
		log:      log,
		sess:     sess,
		sender:   sender,
		pub:      &http.Server{Addr: cfg.HTTPAddr, ReadHeaderTimeout: 10 * time.Second},
		internal: &http.Server{Addr: cfg.InternalAddr, ReadHeaderTimeout: 10 * time.Second},
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
			// Push BEFORE the callback: the operator's write-back reads the task
			// branch on receipt of the callback, so the branch must already carry
			// the agent's commits. A push failure is logged but must not drop the
			// callback (the operator still needs to learn the turn finished).
			if cfg.TaskBranch != "" {
				pushStart := time.Now()
				var err error
				if len(cfg.Repos) > 0 {
					err = bootstrap.CommitAndPushAll(cfg.Workspace, cfg.Repos, cfg.TaskBranch, "tatara agent: "+cfg.TaskBranch, gitRunner())
				} else {
					// Single-repo clones into workspace/<owner>/<repo>, not the
					// workspace root, so commit/push must target that subdir.
					repoDir := bootstrap.RepoDir(cfg.Workspace, cfg.RepoURL)
					if repoDir == "" {
						err = fmt.Errorf("cannot derive repo dir from REPO_URL %q for commit/push", cfg.RepoURL)
					} else {
						err = bootstrap.CommitAndPush(repoDir, cfg.TaskBranch, "tatara agent: "+cfg.TaskBranch, gitRunner())
					}
				}
				if err != nil {
					m.CommitPushTotal.WithLabelValues("fail").Inc()
					log.Error("commit/push failed", "action", "commit_push", "branch", cfg.TaskBranch, "error", err, "duration_ms", time.Since(pushStart).Milliseconds())
				} else {
					m.CommitPushTotal.WithLabelValues("ok").Inc()
					log.Info("commit/push succeeded", "action", "commit_push", "branch", cfg.TaskBranch, "duration_ms", time.Since(pushStart).Milliseconds())
				}
			}

			url := rec.CallbackURL
			if url == "" {
				url = defaultCB
			}
			sender.Deliver(url, rec)
		}()
	}

	sess.StartTailer(ctx)

	if err := sess.Start(ctx); err != nil {
		return nil, err
	}

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
	// Drain in-flight webhook deliveries within a bounded window so retries
	// either complete or log a clean abort instead of being orphaned at exit.
	drainCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	a.sender.Shutdown(drainCtx)
	cancel()
	_ = a.internal.Shutdown(ctx)
	return a.pub.Shutdown(ctx)
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
		SkillsSrc:       strings.Split(cfg.SkillsSrcDirs, ":"),
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
		Repos:           cfg.Repos,
		Log:             log,
		M:               m,
	}
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
	env := append(os.Environ(), "TERM=xterm-256color")
	if cfg.HomeDir != "" {
		env = append(env, "HOME="+cfg.HomeDir)
	}
	return env
}

// tataraLookAndRegister checks tatara is on PATH and wires its MCP server.
// Both the LookPath miss and the mcp-config failure are fatal: the agent cannot
// fulfil the operator contract (propose_issue, review_verdict,
// decline_implementation) without this registration.
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
