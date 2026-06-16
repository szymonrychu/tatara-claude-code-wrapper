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

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/httpapi"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

type fakeCtl struct {
	submitID       string
	submitErr      error
	interjectErr   error
	interjected    string
	completed      session.HookResult
	transcriptPath string
}

func (f *fakeCtl) Submit(text, cb string) (string, error) { return f.submitID, f.submitErr }
func (f *fakeCtl) Interject(text string) error            { f.interjected = text; return f.interjectErr }
func (f *fakeCtl) Complete(r session.HookResult) error    { f.completed = r; return nil }
func (f *fakeCtl) Snapshot() session.Snapshot             { return session.Snapshot{State: session.Ready} }
func (f *fakeCtl) TranscriptPath() string                 { return f.transcriptPath }
func (f *fakeCtl) Alive() bool                            { return true }
func (f *fakeCtl) Shutdown(context.Context) error         { return nil }

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

func TestPostInterject_202(t *testing.T) {
	ctl := &fakeCtl{}
	api := newAPI(ctl, turn.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/v1/interject", bytes.NewReader([]byte(`{"text":"new info"}`)))
	rec := httptest.NewRecorder()
	api.TestRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)
	require.Equal(t, "new info", ctl.interjected)
}

func TestPostInterject_400WhenEmpty(t *testing.T) {
	api := newAPI(&fakeCtl{}, turn.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/v1/interject", bytes.NewReader([]byte(`{"text":""}`)))
	rec := httptest.NewRecorder()
	api.TestRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostInterject_409WhenNotBusy(t *testing.T) {
	api := newAPI(&fakeCtl{interjectErr: session.ErrNotBusy}, turn.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/v1/interject", bytes.NewReader([]byte(`{"text":"x"}`)))
	rec := httptest.NewRecorder()
	api.TestRouter().ServeHTTP(rec, req)
	require.Equal(t, http.StatusConflict, rec.Code)
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
// values that would enable SSRF (finding 2): non-https schemes, loopback, and
// link-local/metadata addresses must all return 400.
func TestPostMessage_SSRFValidation(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"http scheme", "http://operator.example.com/cb"},
		{"loopback IPv4", "https://127.0.0.1/cb"},
		{"loopback localhost", "https://localhost/cb"},
		{"link-local metadata", "https://169.254.169.254/latest/meta-data/"},
		{"link-local other", "https://169.254.1.1/cb"},
		{"private 10.x", "https://10.0.0.1/cb"},
		{"private 172.16.x", "https://172.16.0.1/cb"},
		{"private 192.168.x", "https://192.168.1.1/cb"},
		{"loopback IPv6", "https://[::1]/cb"},
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

// newAPIWithLog builds an API that writes structured logs to logBuf.
func newAPIWithLog(ctl httpapi.SessionController, store *turn.Store, logBuf io.Writer) *httpapi.API {
	log := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return httpapi.New(httpapi.Deps{Ctl: ctl, Store: store, Log: log})
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
