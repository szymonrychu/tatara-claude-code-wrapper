package main

import (
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
