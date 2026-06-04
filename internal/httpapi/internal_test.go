package httpapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/httpapi"
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

var _ httpapi.SessionController = (*fakeCtl)(nil)
