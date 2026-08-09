package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/httpapi"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

func interruptAPI(ctl *fakeCtl) http.Handler {
	return httpapi.New(httpapi.Deps{Ctl: ctl, Store: turn.NewStore()}).TestRouter()
}

// TestPostInterrupt_AcceptedAndCallsControllerExactlyOnce. One request must
// produce exactly one ESC: the byte is a cancel, and a duplicate lands on
// whatever the CLI is doing next - which after a successful interrupt is the
// operator's own handoff turn.
func TestPostInterrupt_AcceptedAndCallsControllerExactlyOnce(t *testing.T) {
	ctl := &fakeCtl{interruptTurnID: "turn-7"}
	rec := httptest.NewRecorder()
	interruptAPI(ctl).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/interrupt", nil))

	require.Equal(t, http.StatusAccepted, rec.Code)
	require.Equal(t, 1, ctl.interrupts)

	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "turn-7", body["turnId"])
}

// TestPostInterrupt_IdleSessionStillAccepted: the operator can lose the race
// with a turn finishing on its own, and that is not an error condition.
func TestPostInterrupt_IdleSessionStillAccepted(t *testing.T) {
	ctl := &fakeCtl{interruptTurnID: ""}
	rec := httptest.NewRecorder()
	interruptAPI(ctl).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/interrupt", nil))

	require.Equal(t, http.StatusAccepted, rec.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Empty(t, body["turnId"])
}

// TestPostInterrupt_NoLivePTYIs503 - retryable, never 409. A 409 would mean
// "a turn is in flight", which is the only state in which interrupting is
// worth doing.
func TestPostInterrupt_NoLivePTYIs503(t *testing.T) {
	ctl := &fakeCtl{interruptErr: session.ErrInterruptUnavailable}
	rec := httptest.NewRecorder()
	interruptAPI(ctl).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/interrupt", nil))

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.NotEqual(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), "interrupt unavailable")
}

// TestPostInterrupt_IgnoresRequestBody: there is exactly one thing to say
// through this channel and no parameters to say it with. A body must neither
// be required nor be able to make the request fail.
func TestPostInterrupt_IgnoresRequestBody(t *testing.T) {
	for _, body := range []string{"", "{}", `{"text":"nonsense"}`, "not json at all"} {
		ctl := &fakeCtl{interruptTurnID: "turn-1"}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/interrupt", strings.NewReader(body))
		interruptAPI(ctl).ServeHTTP(rec, req)
		require.Equal(t, http.StatusAccepted, rec.Code, "body %q", body)
		require.Equal(t, 1, ctl.interrupts)
	}
}
