package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
)

type config struct {
	HTTPAddr            string
	InternalAddr        string
	OIDCIssuer          string
	OIDCAudience        string
	LogLevel            string
	Model               string
	PermissionMode      string
	RepoURL             string
	RepoBranch          string
	GitToken            string
	GitUserName         string
	GitUserEmail        string
	TaskBranch          string
	DefaultCallbackURL  string
	TurnTimeoutSeconds  int
	BootTimeoutSeconds  int
	WebhookRetries      int
	Workspace           string
	HomeDir             string
	ClaudePath          string
	HookPath            string
	GlobalClaudeMdPath  string
	ProjectClaudeMdPath string
	MCPBasePath         string
	MCPOverlayDir       string
	SkillsSrcDirs       string // colon-separated
	AllowedToolsPath    string
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
	cfg := config{
		HTTPAddr:            envOr("HTTP_ADDR", ":8080"),
		InternalAddr:        envOr("INTERNAL_ADDR", "127.0.0.1:8090"),
		OIDCIssuer:          envOr("OIDC_ISSUER", "https://auth.szymonrichert.pl/realms/master"),
		OIDCAudience:        envOr("OIDC_AUDIENCE", "tatara-claude-code-wrapper"),
		LogLevel:            envOr("LOG_LEVEL", "info"),
		Model:               envOr("MODEL", ""),
		PermissionMode:      envOr("PERMISSION_MODE", "bypassPermissions"),
		RepoURL:             envOr("REPO_URL", ""),
		RepoBranch:          envOr("REPO_BRANCH", ""),
		GitToken:            envOr("GIT_TOKEN", ""),
		GitUserName:         envOr("GIT_USER_NAME", "tatara-agent"),
		GitUserEmail:        envOr("GIT_USER_EMAIL", "tatara-agent@szymonrichert.pl"),
		TaskBranch:          envOr("TASK_BRANCH", ""),
		DefaultCallbackURL:  envOr("DEFAULT_CALLBACK_URL", ""),
		TurnTimeoutSeconds:  ti,
		BootTimeoutSeconds:  bt,
		WebhookRetries:      wr,
		Workspace:           envOr("WORKSPACE", "/workspace"),
		HomeDir:             envOr("HOME_DIR", os.Getenv("HOME")),
		ClaudePath:          envOr("CLAUDE_PATH", "claude"),
		HookPath:            envOr("HOOK_PATH", "/usr/local/bin/cc-stop-hook"),
		GlobalClaudeMdPath:  envOr("GLOBAL_CLAUDE_MD_PATH", "/etc/wrapper/global-claude.md"),
		ProjectClaudeMdPath: envOr("PROJECT_CLAUDE_MD_PATH", "/etc/wrapper/project-claude.md"),
		MCPBasePath:         envOr("MCP_BASE_PATH", "/etc/wrapper/mcp-base.json"),
		MCPOverlayDir:       envOr("MCP_OVERLAY_DIR", "/etc/wrapper/mcp.d"),
		SkillsSrcDirs:       envOr("SKILLS_SRC_DIRS", "/templates/skills:/etc/wrapper/skills"),
		AllowedToolsPath:    envOr("ALLOWED_TOOLS_PATH", "/etc/wrapper/allowed-tools.txt"),
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
