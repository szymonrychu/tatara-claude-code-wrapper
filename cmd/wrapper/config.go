package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/storage"
)

type config struct {
	HTTPAddr            string
	InternalAddr        string
	OIDCIssuer          string
	OIDCAudience        string
	LogLevel            string
	Model               string
	Effort              string
	PermissionMode      string
	RepoURL             string
	RepoBranch          string
	GitToken            string
	GitUserName         string
	GitUserEmail        string
	TaskBranch          string
	DefaultCallbackURL  string
	OperatorPushURL     string
	RunID               string
	PodName             string
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
	SkillsSrcDirs       string // colon-separated
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

	// S3 conversation persistence (issue #114). Off unless S3Bucket is set.
	// The operator injects these from a ConfigMap (endpoint/bucket/region/
	// prefix/path-style) plus a k8s secret (the AWS_* creds).
	S3Endpoint       string
	S3Bucket         string
	S3Region         string
	S3KeyPrefix      string
	S3ForcePathStyle bool
	S3AccessKeyID    string
	S3SecretKey      string

	// Conversation resume (issue #114). The operator sets the S3 object key for
	// this issue's conversation (stable per issue); the sessionId is set only
	// when a prior conversation exists, triggering restore + `claude --resume`.
	ConversationObjectKey string
	ConversationSessionID string
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
	fps, err := envBoolOr("S3_FORCE_PATH_STYLE", false)
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
		DefaultCallbackURL:  envOr("DEFAULT_CALLBACK_URL", ""),
		OperatorPushURL:     envOr("OPERATOR_PUSH_URL", ""),
		RunID:               envOr("RUN_ID", ""),
		PodName:             envOr("POD_NAME", ""),
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
		SkillsSrcDirs:       envOr("SKILLS_SRC_DIRS", "/templates/skills:/etc/wrapper/skills"),
		AllowedToolsPath:    envOr("ALLOWED_TOOLS_PATH", "/etc/wrapper/allowed-tools.txt"),

		HookPreClone:             envOr("HOOK_PRE_CLONE", ""),
		HookPostClone:            envOr("HOOK_POST_CLONE", ""),
		HookConversationStart:    envOr("HOOK_CONVERSATION_START", ""),
		HookConversationRestart:  envOr("HOOK_CONVERSATION_RESTART", ""),
		HookAgentTurnFinished:    envOr("HOOK_AGENT_TURN_FINISHED", ""),
		HookConversationFinished: envOr("HOOK_CONVERSATION_FINISHED", ""),

		S3Endpoint:       envOr("S3_ENDPOINT", ""),
		S3Bucket:         envOr("S3_BUCKET", ""),
		S3Region:         envOr("S3_REGION", ""),
		S3KeyPrefix:      envOr("S3_KEY_PREFIX", ""),
		S3ForcePathStyle: fps,
		S3AccessKeyID:    envOr("AWS_ACCESS_KEY_ID", ""),
		S3SecretKey:      envOr("AWS_SECRET_ACCESS_KEY", ""),

		ConversationObjectKey: envOr("CONVERSATION_OBJECT_KEY", ""),
		ConversationSessionID: envOr("CONVERSATION_SESSION_ID", ""),
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
	if !ok {
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
	if !ok {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("env %s: %w", k, err)
	}
	return b, nil
}

// S3Config maps the wrapper config to the storage client config. The
// upload/restore wiring (subtask 4) constructs the client from this when
// storage.Config.Enabled() (i.e. a bucket is set).
func (c config) S3Config() storage.Config {
	return storage.Config{
		Endpoint:       c.S3Endpoint,
		Bucket:         c.S3Bucket,
		Region:         c.S3Region,
		KeyPrefix:      c.S3KeyPrefix,
		ForcePathStyle: c.S3ForcePathStyle,
		AccessKeyID:    c.S3AccessKeyID,
		SecretKey:      c.S3SecretKey,
	}
}
