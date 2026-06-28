package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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

func TestLoadConfig_S3DefaultsOff(t *testing.T) {
	cfg, err := loadConfig(nil)
	require.NoError(t, err)
	require.Equal(t, "", cfg.S3Bucket)
	require.False(t, cfg.S3ForcePathStyle)
	require.False(t, cfg.S3Config().Enabled(), "no bucket => persistence off")
}

func TestLoadConfig_S3FromEnv(t *testing.T) {
	t.Setenv("S3_ENDPOINT", "http://rook-ceph-rgw.tatara.svc")
	t.Setenv("S3_BUCKET", "tatara-conversations")
	t.Setenv("S3_REGION", "us-east-1")
	t.Setenv("S3_KEY_PREFIX", "conv")
	t.Setenv("S3_FORCE_PATH_STYLE", "true")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA_TEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	cfg, err := loadConfig(nil)
	require.NoError(t, err)
	sc := cfg.S3Config()
	require.True(t, sc.Enabled())
	require.Equal(t, "http://rook-ceph-rgw.tatara.svc", sc.Endpoint)
	require.Equal(t, "tatara-conversations", sc.Bucket)
	require.Equal(t, "us-east-1", sc.Region)
	require.Equal(t, "conv", sc.KeyPrefix)
	require.True(t, sc.ForcePathStyle)
	require.Equal(t, "AKIA_TEST", sc.AccessKeyID)
	require.Equal(t, "secret", sc.SecretKey)
}

func TestLoadConfig_S3ForcePathStyleInvalid(t *testing.T) {
	t.Setenv("S3_FORCE_PATH_STYLE", "notabool")
	_, err := loadConfig(nil)
	require.Error(t, err)
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
