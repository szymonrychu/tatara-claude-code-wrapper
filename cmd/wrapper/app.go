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
	"time"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/auth"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/httpapi"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/obs"
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
}

func newApp(ctx context.Context, cfg config) (*app, error) {
	log := obs.NewLogger(os.Stdout, parseLevel(cfg.LogLevel))
	reg := obs.PromRegistry()
	m := metrics.New(reg)

	if err := bootstrap.Render(buildBootstrapParams(cfg), gitRunner()); err != nil {
		return nil, err
	}
	if os.Getenv("TATARA_MEMORY_URL") != "" {
		if _, lookErr := exec.LookPath("tatara"); lookErr == nil {
			if err := bootstrap.RegisterTataraMCP(cfg.Workspace, execRunner(log)); err != nil {
				log.Error("tatara mcp-config failed", "error", err)
			}
		}
	}

	store := turn.NewStore()
	sess := session.New(session.Config{
		ClaudePath:  cfg.ClaudePath,
		Workspace:   cfg.Workspace,
		Env:         claudeEnv(cfg),
		Model:       cfg.Model,
		TurnTimeout: time.Duration(cfg.TurnTimeoutSeconds) * time.Second,
		BootTimeout: time.Duration(cfg.BootTimeoutSeconds) * time.Second,
		SubmitSeq:   session.DefaultSubmitSeq,
	}, store, m, log, time.Now, newTurnID)

	sender := webhook.New(webhook.Config{Retries: cfg.WebhookRetries}, m, log)
	defaultCB := cfg.DefaultCallbackURL
	sess.OnTurnDone = func(rec *turn.Record) {
		// Enforce the branch+commit+push contract the agent is asked to follow
		// but does not reliably perform, so the operator's write-back finds the
		// branch. Best-effort: a failure here must not drop the turn callback.
		if cfg.TaskBranch != "" {
			if len(cfg.Repos) > 0 {
				if err := bootstrap.CommitAndPushAll(cfg.Workspace, cfg.Repos, cfg.TaskBranch, "tatara agent: "+cfg.TaskBranch, gitRunner()); err != nil {
					log.Error("commit/push failed", "error", err)
				}
			} else {
				if err := bootstrap.CommitAndPush(cfg.Workspace, cfg.TaskBranch, "tatara agent: "+cfg.TaskBranch, gitRunner()); err != nil {
					log.Error("commit/push task branch failed", "branch", cfg.TaskBranch, "error", err)
				}
			}
		}
		url := rec.CallbackURL
		if url == "" {
			url = defaultCB
		}
		sender.Deliver(url, rec)
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

	api := httpapi.New(httpapi.Deps{Ctl: sess, Store: store, Verifier: verifier, Log: log, Registry: reg})
	return &app{
		log:      log,
		sess:     sess,
		sender:   sender,
		pub:      &http.Server{Addr: cfg.HTTPAddr, Handler: api.Router(), ReadHeaderTimeout: 10 * time.Second},
		internal: &http.Server{Addr: cfg.InternalAddr, Handler: api.InternalRouter(), ReadHeaderTimeout: 10 * time.Second},
	}, nil
}

func (a *app) run() error {
	errCh := make(chan error, 2)
	go func() { errCh <- a.internal.ListenAndServe() }()
	go func() { errCh <- a.pub.ListenAndServe() }()
	return <-errCh
}

func (a *app) shutdown(ctx context.Context) error {
	_ = a.sess.Shutdown(ctx)
	// Drain in-flight webhook deliveries within a bounded window so retries
	// either complete or log a clean abort instead of being orphaned at exit.
	drainCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	a.sender.Shutdown(drainCtx)
	cancel()
	_ = a.internal.Shutdown(ctx)
	return a.pub.Shutdown(ctx)
}

func buildBootstrapParams(cfg config) bootstrap.Params {
	return bootstrap.Params{
		HomeDir:         cfg.HomeDir,
		Workspace:       cfg.Workspace,
		GlobalClaudeMd:  readFileOrEmpty(cfg.GlobalClaudeMdPath),
		ProjectClaudeMd: readFileOrEmpty(cfg.ProjectClaudeMdPath),
		BaseMCP:         readBytesOrDefault(cfg.MCPBasePath, []byte(`{"mcpServers":{}}`)),
		MCPOverlayDir:   cfg.MCPOverlayDir,
		SkillsSrc:       strings.Split(cfg.SkillsSrcDirs, ":"),
		HookCommand:     cfg.HookPath,
		AllowedTools:    readLines(cfg.AllowedToolsPath),
		EnableAllMCP:    true,
		PermissionMode:  cfg.PermissionMode,
		AnthropicAPIKey: os.Getenv("ANTHROPIC_API_KEY"),
		RepoURL:         cfg.RepoURL,
		RepoBranch:      cfg.RepoBranch,
		GitToken:        cfg.GitToken,
		GitUserName:     cfg.GitUserName,
		GitUserEmail:    cfg.GitUserEmail,
		TaskBranch:      cfg.TaskBranch,
		Repos:           cfg.Repos,
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

func gitRunner() bootstrap.GitRunner {
	return func(dir string, args ...string) error {
		cmd := exec.Command("git", args...) //nolint:gosec // git is a fixed binary; args are operator-supplied config, not user input
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git -C %s %v: %v: %w", dir, args, string(out), err)
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
