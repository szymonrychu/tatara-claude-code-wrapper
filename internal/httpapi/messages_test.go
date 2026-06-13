package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/httpapi"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

type fakeCtl struct {
	submitID  string
	submitErr error
	completed session.HookResult
	tailOut   string
	tailN     int
}

func (f *fakeCtl) Submit(text, cb string) (string, error) { return f.submitID, f.submitErr }
func (f *fakeCtl) Complete(r session.HookResult) error    { f.completed = r; return nil }
func (f *fakeCtl) Snapshot() session.Snapshot             { return session.Snapshot{State: session.Ready} }
func (f *fakeCtl) TranscriptPath() string                 { return "" }
func (f *fakeCtl) Tail(n int) string                      { f.tailN = n; return f.tailOut }
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

func TestGetPTY(t *testing.T) {
	cases := []struct {
		name  string
		query string
		wantN int
	}{
		{"default", "/v1/pty", 4096},
		{"explicit bytes", "/v1/pty?bytes=128", 128},
		{"clamped to max", "/v1/pty?bytes=999999", 64 * 1024},
		{"non-numeric falls back", "/v1/pty?bytes=abc", 4096},
		{"non-positive falls back", "/v1/pty?bytes=0", 4096},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctl := &fakeCtl{tailOut: "TUI output"}
			api := newAPI(ctl, turn.NewStore())
			req := httptest.NewRequest(http.MethodGet, tc.query, nil)
			rec := httptest.NewRecorder()
			api.TestRouter().ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"))
			require.Equal(t, "TUI output", rec.Body.String())
			require.Equal(t, tc.wantN, ctl.tailN)
		})
	}
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
