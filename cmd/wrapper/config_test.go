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
