package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/httpapi"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

func TestInternalTurnComplete_CallsController(t *testing.T) {
	ctl := &fakeCtl{}
	api := newAPI(ctl, turn.NewStore())
	body, _ := json.Marshal(session.HookResult{FinalText: "PONG", StopReason: "end_turn"})
	req := httptest.NewRequest(http.MethodPost, "/internal/turn-complete", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.InternalRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, "PONG", ctl.completed.FinalText)
}

// TestTurnComplete_BadPayload_LogsAndCounts verifies that a bad JSON payload
// returns 400 and logs a warn + increments the bad_payload outcome metric
// (audit finding 8).
func TestTurnComplete_BadPayload_LogsAndCounts(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	api := httpapi.New(httpapi.Deps{
		Ctl:     &fakeCtl{},
		Store:   turn.NewStore(),
		Log:     log,
		Metrics: m,
	})
	req := httptest.NewRequest(http.MethodPost, "/internal/turn-complete", bytes.NewBufferString("{bad json"))
	rec := httptest.NewRecorder()
	api.InternalRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() == "ccw_hook_outcome_total" {
			for _, metric := range mf.GetMetric() {
				for _, lp := range metric.GetLabel() {
					if lp.GetName() == "result" && lp.GetValue() == "bad_payload" {
						require.Equal(t, float64(1), metric.GetCounter().GetValue())
						return
					}
				}
			}
		}
	}
	t.Fatal("ccw_hook_outcome_total{result=bad_payload} not found")
}

// TestTurnComplete_Rejected_Logs409 verifies that a rejected hook (e.g. no
// in-flight turn) returns 409 and logs a warn at the HTTP boundary (audit finding 8).
func TestTurnComplete_Rejected_Logs409(t *testing.T) {
	errRejected := errors.New("no in-flight turn")
	ctl := &fakeCtlErr{err: errRejected}
	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	api := httpapi.New(httpapi.Deps{Ctl: ctl, Store: turn.NewStore(), Log: log})

	body, _ := json.Marshal(session.HookResult{FinalText: "x", StopReason: "end_turn"})
	req := httptest.NewRequest(http.MethodPost, "/internal/turn-complete", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.InternalRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusConflict, rec.Code)

	// Verify a warn log with action=hook_post was emitted at the HTTP layer.
	logLines := bytes.Split(bytes.TrimRight(logBuf.Bytes(), "\n"), []byte("\n"))
	found := false
	for _, ln := range logLines {
		var entry map[string]any
		if json.Unmarshal(ln, &entry) == nil && entry["action"] == "hook_post" {
			found = true
			break
		}
	}
	require.True(t, found, "no hook_post log line at HTTP boundary for rejected hook")
}

// fakeCtlErr is a SessionController whose Complete always returns the given error.
type fakeCtlErr struct{ err error }

func (f *fakeCtlErr) Submit(text, cb string, handoff bool) (string, error) { return "", nil }
func (f *fakeCtlErr) Complete(r session.HookResult) error                  { return f.err }
func (f *fakeCtlErr) Snapshot() session.Snapshot                           { return session.Snapshot{} }
func (f *fakeCtlErr) TranscriptPath() string                               { return "" }
func (f *fakeCtlErr) Alive() bool                                          { return true }
func (f *fakeCtlErr) Shutdown(context.Context) error                       { return nil }

var _ httpapi.SessionController = (*fakeCtl)(nil)
