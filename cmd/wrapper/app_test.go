package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClaudeEnv_OtelEnabledWithEndpoint(t *testing.T) {
	cfg := config{OtelEnabled: true, OtelEndpoint: "otel-collector:4317"}
	env := claudeEnv(cfg)
	require.Contains(t, env, "CLAUDE_CODE_ENABLE_TELEMETRY=1")
	require.Contains(t, env, "OTEL_METRICS_EXPORTER=otlp")
	require.Contains(t, env, "OTEL_LOGS_EXPORTER=otlp")
	require.Contains(t, env, "OTEL_EXPORTER_OTLP_PROTOCOL=grpc")
	require.Contains(t, env, "OTEL_EXPORTER_OTLP_ENDPOINT=otel-collector:4317")
	require.Contains(t, env, "OTEL_METRIC_EXPORT_INTERVAL=60000")
}

func TestClaudeEnv_OtelDisabled(t *testing.T) {
	cfg := config{OtelEnabled: false, OtelEndpoint: "otel-collector:4317"}
	env := claudeEnv(cfg)
	requireNoOtelEnv(t, env)
}

func TestClaudeEnv_OtelEnabledButEndpointEmpty(t *testing.T) {
	cfg := config{OtelEnabled: true, OtelEndpoint: ""}
	env := claudeEnv(cfg)
	requireNoOtelEnv(t, env)
}

func requireNoOtelEnv(t *testing.T, env []string) {
	t.Helper()
	for _, e := range env {
		require.NotContains(t, e, "CLAUDE_CODE_ENABLE_TELEMETRY")
		require.NotContains(t, e, "OTEL_METRICS_EXPORTER")
		require.NotContains(t, e, "OTEL_LOGS_EXPORTER")
		require.NotContains(t, e, "OTEL_EXPORTER_OTLP_PROTOCOL")
		require.NotContains(t, e, "OTEL_EXPORTER_OTLP_ENDPOINT")
		require.NotContains(t, e, "OTEL_METRIC_EXPORT_INTERVAL")
	}
}

func TestBuildBootstrapParams_DerivesAgentsSrcFromSkillsCloneDir(t *testing.T) {
	cfg := config{SkillsSrcDirs: "/etc/wrapper/skills/skills"}
	params := buildBootstrapParams(cfg, nil, nil)
	require.Equal(t, []string{"/etc/wrapper/skills/.claude/agents"}, params.AgentsSrc)
}

func TestClaudeEnv_StripsAmbientSubagentModelOverride(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "opus")
	env := claudeEnv(config{})
	for _, e := range env {
		require.NotContains(t, e, "CLAUDE_CODE_SUBAGENT_MODEL")
	}
}

// TestSetGitHubTokenEnv verifies that setGitHubTokenEnv propagates the bot PAT
// to the env vars that mise and aqua honor for authenticated GitHub API calls.
func TestSetGitHubTokenEnv(t *testing.T) {
	tests := []struct {
		name    string
		token   string
		wantSet bool
		wantVal string
	}{
		{ //nolint:gosec // not a real credential: table-driven test fixture
			name:    "sets both vars when token non-empty",
			token:   "test-bot-pat-value",
			wantSet: true,
			wantVal: "test-bot-pat-value",
		},
		{
			name:    "leaves vars unset when token empty",
			token:   "",
			wantSet: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Unsetenv("GITHUB_TOKEN")
			_ = os.Unsetenv("MISE_GITHUB_TOKEN")
			t.Cleanup(func() {
				_ = os.Unsetenv("GITHUB_TOKEN")
				_ = os.Unsetenv("MISE_GITHUB_TOKEN")
			})

			setGitHubTokenEnv(tc.token)

			ghVal, ghOk := os.LookupEnv("GITHUB_TOKEN")
			miseVal, miseOk := os.LookupEnv("MISE_GITHUB_TOKEN")

			if tc.wantSet {
				require.True(t, ghOk, "GITHUB_TOKEN must be set")
				require.Equal(t, tc.wantVal, ghVal)
				require.True(t, miseOk, "MISE_GITHUB_TOKEN must be set")
				require.Equal(t, tc.wantVal, miseVal)
			} else {
				require.False(t, ghOk, "GITHUB_TOKEN must not be set when token is empty")
				require.False(t, miseOk, "MISE_GITHUB_TOKEN must not be set when token is empty")
			}
		})
	}
}
