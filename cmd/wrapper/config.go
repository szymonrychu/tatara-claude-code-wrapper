package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
)

type config struct {
	HTTPAddr       string
	InternalAddr   string
	OIDCIssuer     string
	OIDCAudience   string
	LogLevel       string
	Model          string
	Effort         string
	PermissionMode string
	RepoURL        string
	RepoBranch     string
	GitToken       string
	GitUserName    string
	GitUserEmail   string
	TaskBranch     string
	// CheckoutBranch is a branch to check out read-only after clone when no
	// TaskBranch is set (issue #114 decision 4: an MR review agent works on the
	// PR head but never pushes). TaskBranch takes precedence when both are set.
	CheckoutBranch     string
	DefaultCallbackURL string
	OperatorPushURL    string
	RunID              string
	PodName            string
	// Metric identity labels (component 6): the operator sets these on the pod
	// env so the wrapper's per-turn token/cost metrics attribute spend to a
	// Task kind, repo, and project. Empty for values the operator does not set
	// (e.g. RepoName is empty for project-scoped kinds).
	Kind                string
	RepoName            string
	Project             string
	TurnTimeoutSeconds  int
	BootTimeoutSeconds  int
	PushIntervalSeconds int
	WebhookRetries      int
	Workspace           string
	HomeDir             string
	ClaudePath          string
	HookPath            string
	GlobalClaudeMdPath  string
	ProjectClaudeMdPath string
	MCPBasePath         string
	MCPOverlayDir       string
	GrafanaMCPURL       string
	SerenaMCPURL        string
	SkillsSrcDirs       string // colon-separated source directories
	SkillProfile        string // TATARA_SKILL_PROFILE; empty = install all (fail-open)
	SkillsRepo          string // TATARA_SKILLS_REPO; boot-clone URL
	SkillsRef           string // TATARA_SKILLS_REF; git ref to clone
	AllowedToolsPath    string
	Repos               []bootstrap.RepoSpec

	// Lifecycle hook commands (set by the operator via HOOK_* env vars). Empty
	// means the hook is disabled.
	HookPreClone             string
	HookPostClone            string
	HookConversationStart    string
	HookConversationRestart  string
	HookAgentTurnFinished    string
	HookConversationFinished string

	// FullClone, when true, clones all history and all branches instead of
	// --depth 1. Set by TATARA_WORKSPACE_FULL_CLONE=true; intended for
	// project-scoped pods (brainstorm/incident/refine/healthCheck) that need
	// cross-branch context.
	FullClone bool

	// ConversationObjectKey is the wrapper's stable-per-issue handoff key
	// (handoff-continuation design, component 3). Formerly also the S3
	// conversation-transcript object key (issue #114); the S3 restore/upload
	// path was removed, but the env name is kept unchanged (CONVERSATION_
	// OBJECT_KEY) so the operator needs no change. Surfaced to the agent via
	// the httpapi handoff preamble on this pod's first goal submission.
	ConversationObjectKey string

	// Native Claude Code OpenTelemetry (cost/token/429 backstop). Both must be
	// set for the wrapper to enable it: OtelEnabled alone with an empty endpoint
	// must not turn on telemetry with nowhere to send it.
	OtelEnabled  bool
	OtelEndpoint string
}

func loadConfig(args []string) (config, error) {
	ti, err := envIntOr("TURN_TIMEOUT_SECONDS", 1800)
	if err != nil {
		return config{}, err
	}
	bt, err := envIntOr("BOOT_TIMEOUT_SECONDS", 60)
	if err != nil {
		return config{}, err
	}
	wr, err := envIntOr("WEBHOOK_RETRIES", 3)
	if err != nil {
		return config{}, err
	}
	pi, err := envIntOr("PUSH_INTERVAL_SECONDS", 15)
	if err != nil {
		return config{}, err
	}
	fc, err := envBoolOr("TATARA_WORKSPACE_FULL_CLONE", false)
	if err != nil {
		return config{}, err
	}
	oe, err := envBoolOr("OTEL_ENABLED", false)
	if err != nil {
		return config{}, err
	}
	cfg := config{
		HTTPAddr:            envOr("HTTP_ADDR", ":8080"),
		InternalAddr:        envOr("INTERNAL_ADDR", "127.0.0.1:8090"),
		OIDCIssuer:          envOr("OIDC_ISSUER", "https://auth.szymonrichert.pl/realms/master"),
		OIDCAudience:        envOr("OIDC_AUDIENCE", "tatara-claude-code-wrapper"),
		LogLevel:            envOr("LOG_LEVEL", "info"),
		Model:               envOr("MODEL", ""),
		Effort:              envOr("EFFORT", ""),
		PermissionMode:      envOr("PERMISSION_MODE", "bypassPermissions"),
		RepoURL:             envOr("REPO_URL", ""),
		RepoBranch:          envOr("REPO_BRANCH", ""),
		GitToken:            envOr("GIT_TOKEN", ""),
		GitUserName:         envOr("GIT_USER_NAME", "tatara-agent"),
		GitUserEmail:        envOr("GIT_USER_EMAIL", "tatara-agent@szymonrichert.pl"),
		TaskBranch:          envOr("TASK_BRANCH", ""),
		CheckoutBranch:      envOr("CHECKOUT_BRANCH", ""),
		DefaultCallbackURL:  envOr("DEFAULT_CALLBACK_URL", ""),
		OperatorPushURL:     envOr("OPERATOR_PUSH_URL", ""),
		RunID:               envOr("RUN_ID", ""),
		PodName:             envOr("POD_NAME", ""),
		Kind:                envOr("TATARA_KIND", ""),
		RepoName:            envOr("TATARA_REPO", ""),
		Project:             envOr("TATARA_PROJECT", ""),
		TurnTimeoutSeconds:  ti,
		BootTimeoutSeconds:  bt,
		PushIntervalSeconds: pi,
		WebhookRetries:      wr,
		Workspace:           envOr("WORKSPACE", "/workspace"),
		HomeDir:             envOr("HOME_DIR", os.Getenv("HOME")),
		ClaudePath:          envOr("CLAUDE_PATH", "claude"),
		HookPath:            envOr("HOOK_PATH", "/usr/local/bin/cc-stop-hook"),
		GlobalClaudeMdPath:  envOr("GLOBAL_CLAUDE_MD_PATH", "/etc/wrapper/global-claude.md"),
		ProjectClaudeMdPath: envOr("PROJECT_CLAUDE_MD_PATH", "/etc/wrapper/project-claude.md"),
		MCPBasePath:         envOr("MCP_BASE_PATH", "/etc/wrapper/mcp-base.json"),
		MCPOverlayDir:       envOr("MCP_OVERLAY_DIR", "/etc/wrapper/mcp.d"),
		GrafanaMCPURL:       os.Getenv("TATARA_GRAFANA_MCP_URL"),
		SerenaMCPURL:        os.Getenv("TATARA_SERENA_URL"),
		SkillsSrcDirs:       envOr("SKILLS_SRC_DIRS", "/etc/wrapper/skills/skills"),
		SkillProfile:        os.Getenv("TATARA_SKILL_PROFILE"),
		SkillsRepo:          envOr("TATARA_SKILLS_REPO", "https://github.com/szymonrychu/tatara-agent-skills"),
		SkillsRef:           envOr("TATARA_SKILLS_REF", "main"),
		AllowedToolsPath:    envOr("ALLOWED_TOOLS_PATH", "/etc/wrapper/allowed-tools.txt"),

		HookPreClone:             envOr("HOOK_PRE_CLONE", ""),
		HookPostClone:            envOr("HOOK_POST_CLONE", ""),
		HookConversationStart:    envOr("HOOK_CONVERSATION_START", ""),
		HookConversationRestart:  envOr("HOOK_CONVERSATION_RESTART", ""),
		HookAgentTurnFinished:    envOr("HOOK_AGENT_TURN_FINISHED", ""),
		HookConversationFinished: envOr("HOOK_CONVERSATION_FINISHED", ""),

		FullClone: fc,

		ConversationObjectKey: envOr("CONVERSATION_OBJECT_KEY", ""),

		OtelEnabled:  oe,
		OtelEndpoint: envOr("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
	}
	if raw := os.Getenv("TATARA_REPOS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.Repos); err != nil {
			return config{}, fmt.Errorf("parse TATARA_REPOS: %w", err)
		}
	}
	fs := flag.NewFlagSet("wrapper", flag.ContinueOnError)
	fs.StringVar(&cfg.HTTPAddr, "http-addr", cfg.HTTPAddr, "public HTTP listen address")
	fs.StringVar(&cfg.InternalAddr, "internal-addr", cfg.InternalAddr, "loopback internal listen address")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

func envIntOr(k string, def int) (int, error) {
	v, ok := os.LookupEnv(k)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("env %s: %w", k, err)
	}
	return n, nil
}

func envBoolOr(k string, def bool) (bool, error) {
	v, ok := os.LookupEnv(k)
	if !ok || v == "" {
		// A set-but-empty value (e.g. the operator emits TATARA_WORKSPACE_FULL_CLONE=""
		// for repo-scoped pods) must fall back to the default, not fail ParseBool("").
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("env %s: %w", k, err)
	}
	return b, nil
}
