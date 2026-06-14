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
	require.Equal(t, "", cfg.MetricsPushURL)
	require.Equal(t, 15, cfg.MetricsPushInterval)
	require.Equal(t, "tatara-claude-code-wrapper", cfg.MetricsJob)
	require.NotEmpty(t, cfg.RunID)   // generated when RUN_ID is unset
	require.NotEmpty(t, cfg.PodName) // hostname fallback
}

func TestLoadConfig_MetricsPushEnvOverride(t *testing.T) {
	t.Setenv("METRICS_PUSH_URL", "http://op:8082")
	t.Setenv("METRICS_PUSH_INTERVAL_SECONDS", "5")
	t.Setenv("RUN_ID", "run-fixed")
	t.Setenv("POD_NAME", "wrapper-7")
	cfg, err := loadConfig(nil)
	require.NoError(t, err)
	require.Equal(t, "http://op:8082", cfg.MetricsPushURL)
	require.Equal(t, 5, cfg.MetricsPushInterval)
	require.Equal(t, "run-fixed", cfg.RunID)
	require.Equal(t, "wrapper-7", cfg.PodName)
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
