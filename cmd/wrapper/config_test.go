package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// configEnvKeys is every environment variable loadConfig consults. TestMain
// unsets them all before the suite runs so the default-assertion tests below
// exercise the compiled defaults, not whatever the host environment happens to
// carry. The CI `test` job runs on an in-cluster ARC runner in the same
// namespace as the wrapper, so ambient vars like TATARA_SKILLS_REF and EFFORT
// are present and would otherwise mask the defaults and fail the build.
var configEnvKeys = []string{
	"HTTP_ADDR", "INTERNAL_ADDR", "OIDC_ISSUER", "OIDC_AUDIENCE", "LOG_LEVEL",
	"MODEL", "EFFORT", "PERMISSION_MODE", "REPO_URL", "REPO_BRANCH", "GIT_TOKEN",
	"GIT_USER_NAME", "GIT_USER_EMAIL", "TASK_BRANCH", "CHECKOUT_BRANCH",
	"DEFAULT_CALLBACK_URL", "OPERATOR_PUSH_URL", "RUN_ID", "POD_NAME",
	"TATARA_KIND", "TATARA_REPO", "TATARA_PROJECT", "TURN_TIMEOUT_SECONDS",
	"BOOT_TIMEOUT_SECONDS", "PUSH_INTERVAL_SECONDS", "WEBHOOK_RETRIES",
	"WORKSPACE", "HOME_DIR", "CLAUDE_PATH", "HOOK_PATH", "GLOBAL_CLAUDE_MD_PATH",
	"PROJECT_CLAUDE_MD_PATH", "MCP_BASE_PATH", "MCP_OVERLAY_DIR",
	"TATARA_GRAFANA_MCP_URL", "TATARA_SERENA_URL", "SKILLS_SRC_DIRS",
	"TATARA_SKILL_PROFILE", "TATARA_SKILLS_REPO", "TATARA_SKILLS_REF",
	"ALLOWED_TOOLS_PATH", "TATARA_WORKER_MODEL", "TATARA_WORKER_EFFORT",
	"HOOK_PRE_CLONE", "HOOK_POST_CLONE", "HOOK_CONVERSATION_START",
	"HOOK_CONVERSATION_RESTART", "HOOK_AGENT_TURN_FINISHED",
	"HOOK_CONVERSATION_FINISHED", "TATARA_WORKSPACE_FULL_CLONE",
	"CONVERSATION_OBJECT_KEY", "OTEL_ENABLED", "OTEL_EXPORTER_OTLP_ENDPOINT",
	"TATARA_REPOS",
}

func TestMain(m *testing.M) {
	for _, k := range configEnvKeys {
		_ = os.Unsetenv(k)
	}
	os.Exit(m.Run())
}

func TestLoadConfig_Defaults(t *testing.T) {
	cfg, err := loadConfig(nil)
	require.NoError(t, err)
	require.Equal(t, ":8080", cfg.HTTPAddr)
	require.Equal(t, "127.0.0.1:8090", cfg.InternalAddr)
	require.Equal(t, "tatara-claude-code-wrapper", cfg.OIDCAudience)
	require.Equal(t, "bypassPermissions", cfg.PermissionMode)
	require.Equal(t, 1800, cfg.TurnTimeoutSeconds)
	require.Equal(t, 3, cfg.WebhookRetries)
}

func TestLoadConfig_EnvOverride(t *testing.T) {
	t.Setenv("HTTP_ADDR", ":9000")
	t.Setenv("TURN_TIMEOUT_SECONDS", "42")
	cfg, err := loadConfig(nil)
	require.NoError(t, err)
	require.Equal(t, ":9000", cfg.HTTPAddr)
	require.Equal(t, 42, cfg.TurnTimeoutSeconds)
}

func TestLoadConfig_ParsesTataraRepos(t *testing.T) {
	t.Setenv("TATARA_REPOS", `[{"name":"a","url":"https://h/a","branch":"main"},{"name":"b","url":"https://h/b","branch":"dev"}]`)
	cfg, err := loadConfig(nil)
	require.NoError(t, err)
	require.Len(t, cfg.Repos, 2)
	require.Equal(t, "a", cfg.Repos[0].Name)
	require.Equal(t, "https://h/b", cfg.Repos[1].URL)
	require.Equal(t, "dev", cfg.Repos[1].Branch)
}

func TestLoadConfig_EffortDefaultsEmpty(t *testing.T) {
	cfg, err := loadConfig(nil)
	require.NoError(t, err)
	require.Equal(t, "", cfg.Effort, "EFFORT unset must yield empty (no --effort)")
}

func TestLoadConfig_EffortFromEnv(t *testing.T) {
	t.Setenv("EFFORT", "xhigh")
	cfg, err := loadConfig(nil)
	require.NoError(t, err)
	require.Equal(t, "xhigh", cfg.Effort)
}

func TestLoadConfig_WorkerModelEffortDefaults(t *testing.T) {
	cfg, err := loadConfig(nil)
	require.NoError(t, err)
	require.Equal(t, "sonnet", cfg.WorkerModel)
	require.Equal(t, "low", cfg.WorkerEffort)
}

func TestLoadConfig_WorkerModelEffortFromEnv(t *testing.T) {
	t.Setenv("TATARA_WORKER_MODEL", "haiku")
	t.Setenv("TATARA_WORKER_EFFORT", "medium")
	cfg, err := loadConfig(nil)
	require.NoError(t, err)
	require.Equal(t, "haiku", cfg.WorkerModel)
	require.Equal(t, "medium", cfg.WorkerEffort)
}

// TestLoadConfig_ConversationObjectKeyIsHandoffKey verifies CONVERSATION_
// OBJECT_KEY (kept unchanged as the operator env name; repurposed as the
// handoff-continuation key, spec component 3) still loads correctly.
func TestLoadConfig_ConversationObjectKeyIsHandoffKey(t *testing.T) {
	cfg, err := loadConfig(nil)
	require.NoError(t, err)
	require.Equal(t, "", cfg.ConversationObjectKey)

	t.Setenv("CONVERSATION_OBJECT_KEY", "issue-42")
	cfg, err = loadConfig(nil)
	require.NoError(t, err)
	require.Equal(t, "issue-42", cfg.ConversationObjectKey)
}

func TestLoadConfig_SkillsDefaults(t *testing.T) {
	cfg, err := loadConfig(nil)
	require.NoError(t, err)
	require.Equal(t, "https://github.com/szymonrychu/tatara-agent-skills", cfg.SkillsRepo)
	require.Equal(t, "main", cfg.SkillsRef)
	require.Equal(t, "", cfg.SkillProfile)
	require.Equal(t, "/etc/wrapper/skills/skills", cfg.SkillsSrcDirs)
}

func TestLoadConfig_SkillsFromEnv(t *testing.T) {
	t.Setenv("TATARA_SKILL_PROFILE", "implement")
	t.Setenv("TATARA_SKILLS_REPO", "https://github.com/custom/skills")
	t.Setenv("TATARA_SKILLS_REF", "v1.2.3")
	cfg, err := loadConfig(nil)
	require.NoError(t, err)
	require.Equal(t, "implement", cfg.SkillProfile)
	require.Equal(t, "https://github.com/custom/skills", cfg.SkillsRepo)
	require.Equal(t, "v1.2.3", cfg.SkillsRef)
}

func TestLoadConfig_FullCloneDefaultFalse(t *testing.T) {
	cfg, err := loadConfig(nil)
	require.NoError(t, err)
	require.False(t, cfg.FullClone, "TATARA_WORKSPACE_FULL_CLONE unset must default false")
}

func TestLoadConfig_FullCloneFromEnv(t *testing.T) {
	t.Setenv("TATARA_WORKSPACE_FULL_CLONE", "true")
	cfg, err := loadConfig(nil)
	require.NoError(t, err)
	require.True(t, cfg.FullClone)
}

// The operator emits TATARA_WORKSPACE_FULL_CLONE="" (set-but-empty) for
// repo-scoped pods. ParseBool("") errors, so loadConfig must treat empty as the
// default (false) rather than failing the whole boot.
func TestLoadConfig_FullCloneEmptyStringDefaults(t *testing.T) {
	t.Setenv("TATARA_WORKSPACE_FULL_CLONE", "")
	cfg, err := loadConfig(nil)
	require.NoError(t, err)
	require.False(t, cfg.FullClone)
}

func TestLoadConfig_MetricLabelEnv(t *testing.T) {
	t.Setenv("TATARA_KIND", "review")
	t.Setenv("TATARA_REPO", "tatara-operator")
	t.Setenv("TATARA_PROJECT", "tatara")

	cfg, err := loadConfig(nil)
	require.NoError(t, err)
	require.Equal(t, "review", cfg.Kind)
	require.Equal(t, "tatara-operator", cfg.RepoName)
	require.Equal(t, "tatara", cfg.Project)
}

func TestLoadConfig_MetricLabelEnv_DefaultsEmpty(t *testing.T) {
	t.Setenv("TATARA_KIND", "")
	t.Setenv("TATARA_REPO", "")
	t.Setenv("TATARA_PROJECT", "")

	cfg, err := loadConfig(nil)
	require.NoError(t, err)
	require.Equal(t, "", cfg.Kind)
	require.Equal(t, "", cfg.RepoName)
	require.Equal(t, "", cfg.Project)
}
