package obs_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/obs"
)

func TestLogger_EmitsValidJSON(t *testing.T) {
	var buf bytes.Buffer
	logger := obs.NewLogger(&buf, slog.LevelInfo)
	require.NotNil(t, logger)

	logger.Info("hello", slog.String("request_id", "abc"))

	var got map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Equal(t, "hello", got["msg"])
	require.Equal(t, "abc", got["request_id"])
	require.Equal(t, "INFO", got["level"])
	require.Contains(t, got, "time")
}

func TestDefaultLogger_StableFields(t *testing.T) {
	var buf bytes.Buffer
	base := obs.NewLogger(&buf, slog.LevelInfo)
	req := obs.RequestLogger(base, obs.RequestFields{
		RequestID:  "rid-1",
		User:       "szymon",
		Route:      "/v1/memories",
		Method:     "POST",
		Status:     201,
		DurationMs: 12,
	})
	req.Info("request handled")

	var got map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	require.Equal(t, "rid-1", got["request_id"])
	require.Equal(t, "szymon", got["user"])
	require.Equal(t, "/v1/memories", got["route"])
	require.Equal(t, "POST", got["method"])
	require.EqualValues(t, 201, got["status"])
	require.EqualValues(t, 12, got["duration_ms"])
}
