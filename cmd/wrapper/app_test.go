package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/transcript"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/webhook"
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

// waitForLogHandler is a minimal slog.Handler that closes ready the first time
// a record's message matches want. Used to deterministically wait for the
// transcript Tailer's Follow goroutine to have processed a line, avoiding a
// race between its polling and this test's subsequent drain (mirrors the
// captureHandler-based synchronization in internal/transcript/tailer_test.go).
type waitForLogHandler struct {
	want  string
	ready chan struct{}
	once  sync.Once
}

func (h *waitForLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *waitForLogHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == h.want {
		h.once.Do(func() { close(h.ready) })
	}
	return nil
}
func (h *waitForLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *waitForLogHandler) WithGroup(string) slog.Handler      { return h }

// TestFinalizeTurn_DrainsInternalIssuesIntoRecord verifies the new app.go
// wiring: finalizeTurn drains the transcript tailer's accumulated
// report_internal_issue calls onto rec.InternalIssues before delivering the
// turn callback. Uses a real transcript.Tailer (Task 3) injected into a real
// session.Manager via SetTailerForTest (Task 4's DrainInternalIssues
// delegates to it) and a real webhook.Sender posting to an httptest server,
// so only the NEW app.go wiring added in this task is under test - the
// tailer's own accumulation and the Manager's delegation are already covered
// by their own package tests.
func TestFinalizeTurn_DrainsInternalIssuesIntoRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	line := `{"type":"assistant","uuid":"u1","sessionId":"s1","timestamp":"2026-07-12T00:00:00.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"mcp__tatara__report_internal_issue","input":{"category":"tool_error","severity":"error","description":"the tool blew up","offending_tool":"Bash","resource_id":"res-1"}}]}}`
	require.NoError(t, os.WriteFile(path, []byte(line+"\n"), 0o644))

	wh := &waitForLogHandler{want: "internal issue report", ready: make(chan struct{})}
	tailer := transcript.NewTailer(slog.New(wh), transcript.NewRedactor(nil), func() string { return "turn-1" })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	followDone := make(chan error, 1)
	go func() { followDone <- tailer.Follow(ctx, path) }()

	select {
	case <-wh.ready:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("timed out waiting for the tailer to process the internal issue line")
	}
	cancel()
	<-followDone

	mgr := session.New(session.Config{}, turn.NewStore(), metrics.New(prometheus.NewRegistry()), testLogger(), time.Now, func() string { return "turn-1" })
	mgr.SetTailerForTest(tailer)

	var delivered turn.Record
	gotCh := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&delivered)
		close(gotCh)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := webhook.New(webhook.Config{Retries: 1}, metrics.New(prometheus.NewRegistry()), testLogger())

	a := &app{log: testLogger(), sess: mgr}
	rec := &turn.Record{ID: "turn-1", CallbackURL: srv.URL}
	a.finalizeTurn(rec, config{}, metrics.New(prometheus.NewRegistry()), testLogger(), sender, "")

	select {
	case <-gotCh:
	case <-time.After(2 * time.Second):
		t.Fatal("callback never delivered")
	}

	want := turn.InternalIssueReport{
		Category: "tool_error", Severity: "error", Description: "the tool blew up",
		OffendingTool: "Bash", ResourceID: "res-1",
	}
	if len(delivered.InternalIssues) != 1 || delivered.InternalIssues[0] != want {
		t.Errorf("delivered.InternalIssues = %+v, want [%+v]", delivered.InternalIssues, want)
	}
}
