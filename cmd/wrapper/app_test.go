package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// appendTranscriptLine appends a JSONL line to an existing transcript file.
func appendTranscriptLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	_, err = f.WriteString(line + "\n")
	require.NoError(t, err)
}

// TestFinalizeTurn_DrainsInternalIssuesIntoRecord_DespiteTailerPollLag
// reproduces the tailer-drain-catchup race found by an in-cluster review
// agent on the merged PR #105: internal/transcript.Tailer.Follow is
// poll-based (200ms) - on EOF it sleeps before re-reading. Complete() fires
// OnTurnDone synchronously off the incoming cc-stop-hook POST, which can
// arrive the instant the transcript's final line is flushed to disk. If the
// agent's last tool call of a turn is report_internal_issue, draining before
// the tailer's next poll silently drops the report.
//
// Unlike TestFinalizeTurn_DrainsInternalIssuesIntoRecord (which waits for
// the tailer to have already processed the internal-issue line before
// draining, sidestepping the race entirely), this test only waits for an
// EARLIER line to be processed - proving the tailer has hit EOF and is
// sitting in its poll sleep - then appends the internal-issue line as the
// turn's last line and drains immediately, with no wait for the tailer's
// next poll. This must not drop the report.
func TestFinalizeTurn_DrainsInternalIssuesIntoRecord_DespiteTailerPollLag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	firstLine := `{"type":"assistant","uuid":"u0","sessionId":"s1","timestamp":"2026-07-12T00:00:00.000Z","message":{"role":"assistant","content":[{"type":"text","text":"working on it"}]}}`
	require.NoError(t, os.WriteFile(path, []byte(firstLine+"\n"), 0o644))

	wh := &waitForLogHandler{want: "agent stream", ready: make(chan struct{})}
	tailer := transcript.NewTailer(slog.New(wh), transcript.NewRedactor(nil), func() string { return "turn-1" })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	followDone := make(chan error, 1)
	go func() { followDone <- tailer.Follow(ctx, path) }()

	// Wait only for the FIRST line to be processed - this proves the tailer
	// has hit EOF and is now in its poll-interval sleep, NOT that it has seen
	// the internal-issue line appended below.
	select {
	case <-wh.ready:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("timed out waiting for the tailer to process the first line")
	}

	// Append report_internal_issue as the turn's LAST line, then drain
	// immediately - no sleep, no wait for the tailer's next poll. This
	// mirrors the actual cc-stop-hook timing.
	lastLine := `{"type":"assistant","uuid":"u1","sessionId":"s1","timestamp":"2026-07-12T00:00:01.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"mcp__tatara__report_internal_issue","input":{"category":"tool_error","severity":"error","description":"the tool blew up","offending_tool":"Bash","resource_id":"res-1"}}]}}`
	appendTranscriptLine(t, path, lastLine)

	mgr := session.New(session.Config{}, turn.NewStore(), metrics.New(prometheus.NewRegistry()), testLogger(), time.Now, func() string { return "turn-1" })
	mgr.SetTailerForTest(tailer)
	mgr.SetTranscriptPathForTest(path)

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
	case <-time.After(3 * time.Second):
		t.Fatal("callback never delivered")
	}
	cancel()
	<-followDone

	want := turn.InternalIssueReport{
		Category: "tool_error", Severity: "error", Description: "the tool blew up",
		OffendingTool: "Bash", ResourceID: "res-1",
	}
	if len(delivered.InternalIssues) != 1 || delivered.InternalIssues[0] != want {
		t.Errorf("delivered.InternalIssues = %+v, want [%+v] (report flushed to disk before the Stop hook must never be dropped by tailer poll lag)",
			delivered.InternalIssues, want)
	}
}

// TestFinalizeTurn_OutcomeRepromptDoesNotLoseInternalIssues covers #136's
// pre-mortem 1, a self-documented residual from
// docs/superpowers/plans/2026-07-19-w1-lastcompleted-fallback.md:383
// ("cmd/wrapper/app.go:239's outcome-reprompt early return still drops
// rec.InternalIssues for that turn - separate bug").
//
// The drain used to sit BELOW the outcome-reprompt early return, so a turn
// whose submit_outcome was rejected lost every report_internal_issue call it
// made: the callback for that turn is deliberately suppressed, the accumulator
// is keyed by turn id, and agent pods are not Loki-scraped - the operator
// callback is the only egress. The loss was total and silent, and it was
// dormant only while the re-prompt itself was dead code (the trigger named two
// tools tatara-cli had reaped).
//
// Draining above the early return is necessary but not sufficient: rec is
// discarded on that path. The reports must be carried onto the NEXT turn's
// callback, which is what this test pins.
func TestFinalizeTurn_OutcomeRepromptDoesNotLoseInternalIssues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	lines := []string{
		// The turn reports a platform problem ...
		`{"type":"assistant","uuid":"u1","sessionId":"s1","timestamp":"2026-07-29T12:44:35.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"mcp__tatara__report_internal_issue","input":{"category":"workspace_broken","severity":"warn","description":"no C toolchain in the pod","offending_tool":"Bash (make test)","resource_id":"res-1"}}]}}`,
		// ... then its submit_outcome is rejected client-side by the cli.
		`{"type":"assistant","uuid":"u2","sessionId":"s1","timestamp":"2026-07-29T12:44:36.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu2","name":"mcp__tatara__submit_outcome","input":{"action":"declined"}}]}}`,
		`{"type":"user","uuid":"u3","sessionId":"s1","timestamp":"2026-07-29T12:44:37.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu2","is_error":true,"content":"submit_outcome: decline_reason required (non-empty) when action=declined"}]}}`,
	}
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644))

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
	mgr.SetTranscriptPathForTest(path)

	var delivered turn.Record
	gotCh := make(chan struct{}, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&delivered)
		gotCh <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := webhook.New(webhook.Config{Retries: 1}, metrics.New(prometheus.NewRegistry()), testLogger())

	var submits int
	a := &app{log: testLogger(), sess: mgr, submitFn: func(text, cb string) (string, error) {
		submits++
		require.Contains(t, text, "submit_outcome")
		return "turn-2", nil
	}}

	// Turn 1: rejected submit_outcome -> re-prompt, callback suppressed.
	a.finalizeTurn(&turn.Record{ID: "turn-1", CallbackURL: srv.URL},
		config{}, metrics.New(prometheus.NewRegistry()), testLogger(), sender, "")
	require.Equal(t, 1, submits, "a rejected submit_outcome must re-prompt the agent")
	select {
	case <-gotCh:
		t.Fatal("turn 1's callback must be suppressed while the agent is re-prompted")
	case <-time.After(300 * time.Millisecond):
	}

	// Turn 2: the re-prompted turn succeeds. Its callback must carry turn 1's
	// report, which has nowhere else to go.
	mgr.SetTranscriptPathForTest("")
	a.finalizeTurn(&turn.Record{ID: "turn-2", CallbackURL: srv.URL},
		config{}, metrics.New(prometheus.NewRegistry()), testLogger(), sender, "")
	select {
	case <-gotCh:
	case <-time.After(2 * time.Second):
		t.Fatal("turn 2's callback never delivered")
	}

	want := turn.InternalIssueReport{
		Category: "workspace_broken", Severity: "warn", Description: "no C toolchain in the pod",
		OffendingTool: "Bash (make test)", ResourceID: "res-1",
	}
	require.Equalf(t, []turn.InternalIssueReport{want}, delivered.InternalIssues,
		"a report filed during a re-prompted turn must reach the operator on the next callback; "+
			"the suppressed turn's callback is its only egress")
}
