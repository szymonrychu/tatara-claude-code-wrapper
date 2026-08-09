package httpapi_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/transcript"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

// TestPostProbe_Table covers the whole status-code surface of POST /v1/probe.
//
// The case that matters most is "busy session": the entire reason this
// endpoint exists rather than a flag on POST /v1/messages is that /v1/messages
// answers 409 for the whole duration of a turn, which is exactly when a probe
// is worth sending. There is deliberately no path here that can produce a 409.
func TestPostProbe_Table(t *testing.T) {
	tests := []struct {
		name     string
		ctl      *fakeCtl
		body     string
		wantCode int
		wantBody string
	}{
		{
			name:     "accepted with explicit text",
			ctl:      &fakeCtl{probeID: "probe-1"},
			body:     `{"text":"still there? Reply with exactly TATARA-ALIVE ..."}`,
			wantCode: http.StatusAccepted,
			wantBody: "probe-1",
		},
		{
			name:     "accepted with empty body (wrapper supplies the default text)",
			ctl:      &fakeCtl{probeID: "probe-2"},
			body:     ``,
			wantCode: http.StatusAccepted,
			wantBody: "probe-2",
		},
		{
			name:     "accepted with no text field",
			ctl:      &fakeCtl{probeID: "probe-3"},
			body:     `{}`,
			wantCode: http.StatusAccepted,
			wantBody: "probe-3",
		},
		{
			name:     "malformed body is 400",
			ctl:      &fakeCtl{probeID: "probe-4"},
			body:     `{"text":`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "no live pty is 503, never 409",
			ctl:      &fakeCtl{probeErr: session.ErrProbeUnavailable},
			body:     `{"text":"hi"}`,
			wantCode: http.StatusServiceUnavailable,
		},
		{
			name:     "write failure is 503, never 409",
			ctl:      &fakeCtl{probeErr: errors.New("write pty submit: broken pipe")},
			body:     `{"text":"hi"}`,
			wantCode: http.StatusServiceUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ctl.probeID != "" {
				tc.ctl.probeFound = true
				tc.ctl.probeStatus = session.ProbeStatus{ID: tc.ctl.probeID, State: transcript.ProbeStatePending}
			}
			api := newAPI(tc.ctl, turn.NewStore())
			req := httptest.NewRequest(http.MethodPost, "/v1/probe", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			api.TestRouter().ServeHTTP(rec, req)

			require.Equal(t, tc.wantCode, rec.Code)
			require.NotEqual(t, http.StatusConflict, rec.Code,
				"POST /v1/probe must never answer 409 - working while a turn is in flight is the whole point")
			if tc.wantBody != "" {
				require.Contains(t, rec.Body.String(), tc.wantBody)
			}
		})
	}
}

// TestPostProbe_EmptyBodyReachesController asserts an empty body still calls
// Probe (with an empty text, which the session layer turns into
// DefaultProbeText) rather than being rejected as a missing field. The
// operator sends the same probe every time; making it restate the
// TATARA-ALIVE instruction on every call is an avoidable way to lose answers.
func TestPostProbe_EmptyBodyReachesController(t *testing.T) {
	ctl := &fakeCtl{probeID: "probe-1", probeFound: true,
		probeStatus: session.ProbeStatus{ID: "probe-1", State: transcript.ProbeStatePending}}
	api := newAPI(ctl, turn.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/v1/probe", strings.NewReader(``))
	rec := httptest.NewRecorder()
	api.TestRouter().ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)
	require.Equal(t, []string{""}, ctl.probedTexts)
}

// TestGetProbe_Table covers GET /v1/probe/{probeId} across every state the
// tracker can report. The three states are three different diagnoses, so the
// handler must pass them through verbatim rather than flattening them into
// done/not-done.
func TestGetProbe_Table(t *testing.T) {
	sent := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		id       string
		status   session.ProbeStatus
		found    bool
		wantCode int
		wantJSON map[string]any
	}{
		{
			name: "pending, not yet enqueued",
			id:   "p1",
			status: session.ProbeStatus{
				ID: "p1", State: transcript.ProbeStatePending, SentAt: sent,
			},
			found:    true,
			wantCode: http.StatusOK,
			wantJSON: map[string]any{"probeId": "p1", "state": "pending"},
		},
		{
			name: "pending with an enqueue: blocked inside one long tool call",
			id:   "p2",
			status: session.ProbeStatus{
				ID: "p2", State: transcript.ProbeStatePending, SentAt: sent,
				EnqueuedAt: sent.Add(30 * time.Millisecond),
			},
			found:    true,
			wantCode: http.StatusOK,
			wantJSON: map[string]any{"probeId": "p2", "state": "pending"},
		},
		{
			name: "delivered but unanswered",
			id:   "p3",
			status: session.ProbeStatus{
				ID: "p3", State: transcript.ProbeStateDelivered, SentAt: sent,
				EnqueuedAt: sent, DeliveredAt: sent.Add(2 * time.Second),
			},
			found:    true,
			wantCode: http.StatusOK,
			wantJSON: map[string]any{"probeId": "p3", "state": "delivered"},
		},
		{
			name: "answered",
			id:   "p4",
			status: session.ProbeStatus{
				ID: "p4", State: transcript.ProbeStateAnswered, SentAt: sent,
				EnqueuedAt: sent, DeliveredAt: sent.Add(2 * time.Second),
				AnsweredAt: sent.Add(3 * time.Second),
				Answer:     "TATARA-ALIVE running the test suite",
			},
			found:    true,
			wantCode: http.StatusOK,
			wantJSON: map[string]any{
				"probeId": "p4", "state": "answered",
				"answer": "TATARA-ALIVE running the test suite",
			},
		},
		{
			name:     "unknown or superseded id is 404",
			id:       "p5",
			found:    false,
			wantCode: http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctl := &fakeCtl{probeStatus: tc.status, probeFound: tc.found}
			api := newAPI(ctl, turn.NewStore())
			req := httptest.NewRequest(http.MethodGet, "/v1/probe/"+tc.id, nil)
			rec := httptest.NewRecorder()
			api.TestRouter().ServeHTTP(rec, req)

			require.Equal(t, tc.wantCode, rec.Code)
			if tc.wantJSON == nil {
				return
			}
			var got map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			for k, want := range tc.wantJSON {
				require.Equal(t, want, got[k], "field %q", k)
			}
		})
	}
}

// TestGetProbe_PendingOmitsUnsetTimestamps guards the shape the operator
// parses: a pending probe that has not been seen in the transcript yet must
// not report a zero-valued deliveredAt/answeredAt, or a client comparing
// timestamps would read the zero time as "delivered in year 1".
func TestGetProbe_PendingOmitsUnsetTimestamps(t *testing.T) {
	ctl := &fakeCtl{
		probeFound: true,
		probeStatus: session.ProbeStatus{
			ID: "p1", State: transcript.ProbeStatePending,
			SentAt: time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC),
		},
	}
	api := newAPI(ctl, turn.NewStore())
	req := httptest.NewRequest(http.MethodGet, "/v1/probe/p1", nil)
	rec := httptest.NewRecorder()
	api.TestRouter().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.NotContains(t, got, "deliveredAt")
	require.NotContains(t, got, "answeredAt")
	require.NotContains(t, got, "enqueuedAt")
	require.Contains(t, got, "sentAt")
}
