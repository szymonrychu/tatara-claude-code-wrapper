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
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/httpapi"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/version"
)

type fakeCtl struct {
	submitID       string
	submitErr      error
	submitErrs     []error // if set, consumed in order per call; falls back to submitErr once exhausted
	completed      session.HookResult
	transcriptPath string
	submittedTexts []string
	lastHandoff    bool // the handoff flag of the most recent Submit
}

func (f *fakeCtl) Submit(text, cb string, handoff bool) (string, error) {
	f.submittedTexts = append(f.submittedTexts, text)
	f.lastHandoff = handoff
	if len(f.submitErrs) > 0 {
		err := f.submitErrs[0]
		f.submitErrs = f.submitErrs[1:]
		return f.submitID, err
	}
	return f.submitID, f.submitErr
}
func (f *fakeCtl) Complete(r session.HookResult) error { f.completed = r; return nil }
func (f *fakeCtl) Snapshot() session.Snapshot {
	return session.Snapshot{State: session.Ready, ContractVersion: version.ContractVersion}
}
func (f *fakeCtl) TranscriptPath() string         { return f.transcriptPath }
func (f *fakeCtl) Alive() bool                    { return true }
func (f *fakeCtl) Shutdown(context.Context) error { return nil }

func newAPI(ctl httpapi.SessionController, store *turn.Store) *httpapi.API {
	return httpapi.New(httpapi.Deps{Ctl: ctl, Store: store}) // Verifier nil -> public router skips OIDC in test mode
}

func TestPostMessage_202(t *testing.T) {
	store := turn.NewStore()
	api := newAPI(&fakeCtl{submitID: "turn-9"}, store)
	body, _ := json.Marshal(map[string]string{"text": "hi", "callbackUrl": "https://cb/x"})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.TestRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)
	require.Contains(t, rec.Body.String(), "turn-9")
}

func TestPostMessage_409WhenBusy(t *testing.T) {
	api := newAPI(&fakeCtl{submitErr: session.ErrBusy}, turn.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{"text":"x"}`)))
	rec := httptest.NewRecorder()
	api.TestRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusConflict, rec.Code)
}

// TestPostInterject_IsGone: contract G.5. Mid-turn PTY injection races the
// Stop hook and the tailer. Mid-flight events now ride in at the TURN
// BOUNDARY, as the <events> block of the next turn's bundle (contract E.3).
// Leaving the endpoint alive leaves the race reachable.
func TestPostInterject_IsGone(t *testing.T) {
	ctl := &fakeCtl{}
	api := newAPI(ctl, turn.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/v1/interject", strings.NewReader(`{"text":"also handle gitlab"}`))
	rec := httptest.NewRecorder()
	api.TestRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestGetMessage_404Then200(t *testing.T) {
	store := turn.NewStore()
	api := newAPI(&fakeCtl{}, store)

	req := httptest.NewRequest(http.MethodGet, "/v1/messages/none", nil)
	rec := httptest.NewRecorder()
	api.TestRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)

	store.Create("turn-1", "hi", "", timeZero())
	req = httptest.NewRequest(http.MethodGet, "/v1/messages/turn-1", nil)
	rec = httptest.NewRecorder()
	api.TestRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func timeZero() time.Time { return time.Unix(0, 0) }

var _ = errors.New // keep errors import if unused after edits

// TestPostMessage_SSRFValidation verifies that postMessage rejects callbackUrl
// values that would enable SSRF (finding 2): non-http(s) schemes, loopback, and
// link-local/metadata addresses must all return 400. http and https are both
// allowed schemes (in-cluster callbacks are plaintext), so the IP-range guards
// must fire regardless of scheme.
func TestPostMessage_SSRFValidation(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"file scheme", "file:///etc/passwd"},
		{"gopher scheme", "gopher://operator.example.com/cb"},
		{"loopback IPv4", "https://127.0.0.1/cb"},
		{"loopback localhost", "https://localhost/cb"},
		{"link-local metadata https", "https://169.254.169.254/latest/meta-data/"},
		{"link-local metadata http", "http://169.254.169.254/latest/meta-data/"},
		{"loopback IPv4 http", "http://127.0.0.1/cb"},
		{"private 10.x http", "http://10.0.0.1/cb"},
		{"link-local other", "https://169.254.1.1/cb"},
		{"private 10.x", "https://10.0.0.1/cb"},
		{"private 172.16.x", "https://172.16.0.1/cb"},
		{"private 192.168.x", "https://192.168.1.1/cb"},
		{"loopback IPv6", "https://[::1]/cb"},
		{"unique-local IPv6", "https://[fd00::1]/cb"},
		{"unspecified IPv4", "https://0.0.0.0/cb"},
		{"unspecified IPv6", "https://[::]/cb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newAPI(&fakeCtl{submitID: "t"}, turn.NewStore())
			body, _ := json.Marshal(map[string]string{"text": "hi", "callbackUrl": tc.url})
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			api.TestRouter().ServeHTTP(rec, req)
			require.Equal(t, http.StatusBadRequest, rec.Code, "expected 400 for unsafe callbackUrl: %s", tc.url)
		})
	}
}

// TestPostMessage_EmptyCallbackAllowed verifies an empty callbackUrl is accepted
// (finding 2): empty means "use server default", not an attack vector.
func TestPostMessage_EmptyCallbackAllowed(t *testing.T) {
	api := newAPI(&fakeCtl{submitID: "t"}, turn.NewStore())
	body, _ := json.Marshal(map[string]string{"text": "hi", "callbackUrl": ""})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.TestRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)
}

// TestPostMessage_HTTPSCallbackAccepted verifies a valid https callbackUrl passes (finding 2).
func TestPostMessage_HTTPSCallbackAccepted(t *testing.T) {
	api := newAPI(&fakeCtl{submitID: "t"}, turn.NewStore())
	body, _ := json.Marshal(map[string]string{"text": "hi", "callbackUrl": "https://operator.example.com/cb"})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.TestRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)
}

// TestPostMessage_HTTPClusterCallbackAccepted verifies the in-cluster plaintext
// callback the operator actually sends (a ClusterIP svc DNS name, no TLS) passes.
// The https-only rule rejected this and stalled every turn submit.
func TestPostMessage_HTTPClusterCallbackAccepted(t *testing.T) {
	api := newAPI(&fakeCtl{submitID: "t"}, turn.NewStore())
	body, _ := json.Marshal(map[string]string{"text": "hi",
		"callbackUrl": "http://tatara-operator-internal.tatara.svc:8082/internal/turn-complete"})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.TestRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)
}

// TestPostMessage_SubmitsTheGoalTextVerbatim: every pod's turn-0 gets the same
// render from the operator (contract E.2) - there is no continuation preamble
// left to prepend, so the goal text reaches Submit unmodified.
func TestPostMessage_SubmitsTheGoalTextVerbatim(t *testing.T) {
	ctl := &fakeCtl{submitID: "t"}
	api := httpapi.New(httpapi.Deps{Ctl: ctl, Store: turn.NewStore()})

	body1, _ := json.Marshal(map[string]string{"text": "do the thing"})
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body1))
	rec1 := httptest.NewRecorder()
	api.TestRouter().ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusAccepted, rec1.Code)

	body2, _ := json.Marshal(map[string]string{"text": "second goal"})
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body2))
	rec2 := httptest.NewRecorder()
	api.TestRouter().ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusAccepted, rec2.Code)

	require.Equal(t, []string{"do the thing", "second goal"}, ctl.submittedTexts)
}

// TestPostMessage_NoContinuationPreambleOnTheFirstSubmit is the regression
// guard for the continuation/handoff preamble deletion (task-centric plan
// Task 4). Contract E.2: every pod's turn-0 gets the SAME render from the
// operator - there is no resume mode. A preamble that tells the agent to call
// get_handoff names a tool that no longer exists, against a chat service that
// no longer exists. CONVERSATION_OBJECT_KEY must have NO effect: it is no
// longer read anywhere in the wrapper.
func TestPostMessage_NoContinuationPreambleOnTheFirstSubmit(t *testing.T) {
	t.Setenv("CONVERSATION_OBJECT_KEY", "some-key") // must have NO effect
	ctl := &fakeCtl{}
	api := newAPI(ctl, turn.NewStore())
	body := `{"text":"<task_context task=\"t1\">...</task_context>","callbackUrl":"http://cb"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	api.TestRouter().ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)
	require.Equal(t, []string{`<task_context task="t1">...</task_context>`}, ctl.submittedTexts,
		"the submitted text is the operator's bundle, verbatim - nothing is prepended")
	require.NotContains(t, ctl.submittedTexts[0], "Continuation key")
}

// TestPostMessage_503WhenSessionNotReady: a still-booting session is the one
// submit failure that IS worth retrying, so it keeps its 503 (unlike the TTL
// refusal, which is a permanent 410).
func TestPostMessage_503WhenSessionNotReady(t *testing.T) {
	ctl := &fakeCtl{submitID: "t", submitErrs: []error{errors.New("session not ready")}}
	api := newAPI(ctl, turn.NewStore())

	body, _ := json.Marshal(map[string]string{"text": "do the thing"})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.TestRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// newAPIWithLog builds an API that writes structured logs to logBuf.
func newAPIWithLog(ctl httpapi.SessionController, store *turn.Store, logBuf io.Writer) *httpapi.API {
	log := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return httpapi.New(httpapi.Deps{Ctl: ctl, Store: store, Log: log})
}

// newAPIWithLogLevel builds an API whose logger filters at the given level.
func newAPIWithLogLevel(ctl httpapi.SessionController, store *turn.Store, logBuf io.Writer, level slog.Level) *httpapi.API {
	log := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: level}))
	return httpapi.New(httpapi.Deps{Ctl: ctl, Store: store, Log: log})
}

// requestHandledRoutes returns the route field of every "request handled" log
// line in data.
func requestHandledRoutes(t *testing.T, data string) []string {
	t.Helper()
	var routes []string
	for ln := range strings.SplitSeq(strings.TrimRight(data, "\n"), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(ln), &m) == nil && m["msg"] == "request handled" {
			if r, ok := m["route"].(string); ok {
				routes = append(routes, r)
			}
		}
	}
	return routes
}

// TestRouter_ProbeRoutesNotLoggedAtInfo verifies that the kubelet/Prometheus
// probe routes (/healthz, /readyz, /metrics) do NOT emit a "request handled"
// INFO line, so the access log at the default level is not drowned in probe
// noise (every ~10s per probe). Real business requests still log.
func TestRouter_ProbeRoutesNotLoggedAtInfo(t *testing.T) {
	var logBuf bytes.Buffer
	reg := prometheus.NewRegistry()
	api := httpapi.New(httpapi.Deps{
		Ctl:      &aliveCtl{},
		Store:    turn.NewStore(),
		Log:      slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})),
		Registry: reg,
	})

	for _, route := range []string{"/healthz", "/readyz", "/metrics"} {
		req := httptest.NewRequest(http.MethodGet, route, nil)
		api.Router().ServeHTTP(httptest.NewRecorder(), req)
	}

	routes := requestHandledRoutes(t, logBuf.String())
	for _, r := range routes {
		require.NotContains(t, []string{"/healthz", "/readyz", "/metrics"}, r,
			"probe route %q must not emit a request-handled INFO line", r)
	}
}

// TestRouter_ProbeRoutesLoggedAtDebug verifies the probe access logs are only
// demoted, not dropped: at LOG_LEVEL=debug they reappear for troubleshooting.
func TestRouter_ProbeRoutesLoggedAtDebug(t *testing.T) {
	var logBuf bytes.Buffer
	api := newAPIWithLogLevel(&aliveCtl{}, turn.NewStore(), &logBuf, slog.LevelDebug)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	api.Router().ServeHTTP(httptest.NewRecorder(), req)

	require.Contains(t, requestHandledRoutes(t, logBuf.String()), "/readyz",
		"probe route must still log at debug level")
}

// TestRouter_BusinessRouteLoggedAtInfo verifies a real request still emits its
// "request handled" INFO line after the probe demotion.
func TestRouter_BusinessRouteLoggedAtInfo(t *testing.T) {
	var logBuf bytes.Buffer
	api := newAPIWithLog(&fakeCtl{submitID: "turn-x"}, turn.NewStore(), &logBuf)

	body, _ := json.Marshal(map[string]string{"text": "hello", "callbackUrl": ""})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	api.Router().ServeHTTP(httptest.NewRecorder(), req)

	require.Contains(t, requestHandledRoutes(t, logBuf.String()), "/v1/messages",
		"business route must still log at info level")
}

// TestRouter_EmitsRequestHandledLog verifies the logging middleware writes a
// "request handled" JSON log line with method, route, status, duration_ms
// for every request (findings 4+5).
func TestRouter_EmitsRequestHandledLog(t *testing.T) {
	var logBuf bytes.Buffer
	api := newAPIWithLog(&fakeCtl{submitID: "turn-x"}, turn.NewStore(), &logBuf)

	body, _ := json.Marshal(map[string]string{"text": "hello", "callbackUrl": ""})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	// Use Router() - the real public router with middleware
	api.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)

	data := logBuf.String()
	lines := strings.Split(strings.TrimRight(data, "\n"), "\n")
	found := false
	for _, ln := range lines {
		var m map[string]any
		if json.Unmarshal([]byte(ln), &m) == nil && m["msg"] == "request handled" {
			require.Equal(t, "POST", m["method"])
			require.NotNil(t, m["route"])
			require.NotNil(t, m["status"])
			require.NotNil(t, m["duration_ms"])
			found = true
		}
	}
	require.True(t, found, "expected 'request handled' log line from Router middleware")
}

// TestGetTranscript_Streams verifies that GET /v1/transcript streams the file
// without loading it all into memory at once (finding 3). Correctness check:
// response body equals file contents and Content-Type is application/x-ndjson.
func TestGetTranscript_Streams(t *testing.T) {
	// Write a small JSONL file to a temp path.
	f, err := os.CreateTemp(t.TempDir(), "transcript-*.jsonl")
	require.NoError(t, err)
	content := `{"type":"user","text":"hello"}` + "\n" + `{"type":"assistant","text":"world"}` + "\n"
	_, err = io.WriteString(f, content)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	ctl := &fakeCtl{transcriptPath: f.Name()}
	api := newAPI(ctl, turn.NewStore())

	req := httptest.NewRequest(http.MethodGet, "/v1/transcript", nil)
	rec := httptest.NewRecorder()
	api.TestRouter().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/x-ndjson", rec.Header().Get("Content-Type"))
	require.Equal(t, content, rec.Body.String())
}

// TestGetSession_ReportsContractVersion verifies GET /v1/session serves
// contractVersion so the operator can assert it before submitting turn-0
// (contract G.10), and that no existing Snapshot field was dropped (D-W3).
func TestGetSession_ReportsContractVersion(t *testing.T) {
	api := newAPI(&fakeCtl{}, turn.NewStore())
	req := httptest.NewRequest(http.MethodGet, "/v1/session", nil)
	rec := httptest.NewRecorder()
	api.TestRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, float64(2), got["contractVersion"],
		"the operator asserts this BEFORE turn-0; an absent field means an old wrapper (contract G.10)")

	// D-W3: the existing fields survive. The operator reads them.
	for _, k := range []string{"state", "turnsCompleted", "turnsFinished", "model", "repo", "lastActivityAt"} {
		require.Contains(t, got, k, "GET /v1/session must not lose %q", k)
	}
}

// TestInternalRouter_EmitsRequestHandledLog verifies access-log middleware on
// the internal router (finding 5).
func TestInternalRouter_EmitsRequestHandledLog(t *testing.T) {
	var logBuf bytes.Buffer
	ctl := &fakeCtl{}
	api := newAPIWithLog(ctl, turn.NewStore(), &logBuf)

	body, _ := json.Marshal(session.HookResult{FinalText: "ok", StopReason: "end_turn"})
	req := httptest.NewRequest(http.MethodPost, "/internal/turn-complete", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.InternalRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)

	data := logBuf.String()
	lines := strings.Split(strings.TrimRight(data, "\n"), "\n")
	found := false
	for _, ln := range lines {
		var m map[string]any
		if json.Unmarshal([]byte(ln), &m) == nil && m["msg"] == "request handled" {
			found = true
		}
	}
	require.True(t, found, "expected 'request handled' log line from InternalRouter middleware")
}

// TestPostMessage_410WhenPodTTLExpired: the TTL refusal must be a status the
// operator can branch on. 410, NOT 409 (which means "a turn is in flight") and
// NOT 503 (which means "try again shortly"). This pod will never accept another
// ordinary turn (contract G.5, D-W1).
func TestPostMessage_410WhenPodTTLExpired(t *testing.T) {
	api := newAPI(&fakeCtl{submitErr: session.ErrPodTTLExpired}, turn.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"text":"go","callbackUrl":"https://cb/x"}`))
	rec := httptest.NewRecorder()
	api.TestRouter().ServeHTTP(rec, req)

	require.Equal(t, http.StatusGone, rec.Code,
		"a wrapper log line reaches nobody - agent pods are not Loki-scraped. The refusal MUST be a non-2xx the operator can see.")
	require.Contains(t, rec.Body.String(), "pod ttl expired")
}

// TestPostMessage_PassesTheHandoffFlagThrough: the operator marks its TTL
// handoff turn with "handoff":true (contract G.5, wire tag handoff,omitempty);
// the wrapper admits exactly that one turn past the deadline.
func TestPostMessage_PassesTheHandoffFlagThrough(t *testing.T) {
	ctl := &fakeCtl{submitID: "turn-h"}
	api := newAPI(ctl, turn.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"text":"stop","callbackUrl":"https://cb/x","handoff":true}`))
	rec := httptest.NewRecorder()
	api.TestRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)
	require.True(t, ctl.lastHandoff, "the operator marks its TTL handoff turn; the wrapper admits it past the deadline")
}

// TestPostMessage_OrdinaryTurnCarriesNoHandoffFlag: an ordinary turn's body
// omits the key entirely (omitempty), and must reach Submit as handoff=false.
func TestPostMessage_OrdinaryTurnCarriesNoHandoffFlag(t *testing.T) {
	ctl := &fakeCtl{submitID: "turn-o"}
	api := newAPI(ctl, turn.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"text":"work","callbackUrl":"https://cb/x"}`))
	rec := httptest.NewRecorder()
	api.TestRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)
	require.False(t, ctl.lastHandoff)
}
