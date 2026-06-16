package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildResult_FromHookPayload(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("testdata", "hook_payload.json"))
	if err != nil {
		t.Skip("spike fixture missing; run Task 1")
	}
	res, err := buildResult(payload, "/nonexistent/result.json")
	require.NoError(t, err)
	require.NotEmpty(t, res.FinalText)
	require.Equal(t, "PONG", res.FinalText)
}

// TestRun_EmitsJSONLogOnSuccess verifies that run() writes a valid JSON INFO
// log on success with action=hook_post and session_id (finding 1).
func TestRun_EmitsJSONLogOnSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Setenv("CCW_INTERNAL_URL", srv.URL)
	t.Setenv("CCW_RESULT_JSON", "/nonexistent/result.json")

	payload := `{"session_id":"sess-abc","last_assistant_message":"hello","transcript_path":""}`
	origStdin := os.Stdin
	r, w, err := os.Pipe()
	require.NoError(t, err)
	_, _ = w.WriteString(payload)
	require.NoError(t, w.Close())
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	err = run(log)
	require.NoError(t, err)

	data := buf.Bytes()
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	found := false
	for _, ln := range lines {
		var rec map[string]any
		if json.Unmarshal(ln, &rec) == nil && rec["level"] == "INFO" && rec["msg"] == "hook post succeeded" {
			require.Equal(t, "hook_post", rec["action"], "action must be hook_post")
			require.Equal(t, "sess-abc", rec["session_id"], "session_id must be present")
			require.NotNil(t, rec["duration_ms"], "duration_ms must be present")
			found = true
		}
	}
	require.True(t, found, "no INFO 'hook post succeeded' JSON log found")
}

// TestRun_EmitsJSONErrorLogOnFailure verifies that run() writes an ERROR JSON
// log with action=hook_post when all retries fail (finding 1).
func TestRun_EmitsJSONErrorLogOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	t.Setenv("CCW_INTERNAL_URL", srv.URL)
	t.Setenv("CCW_RESULT_JSON", "/nonexistent/result.json")

	payload := `{"session_id":"sess-xyz","last_assistant_message":"err case","transcript_path":""}`
	origStdin := os.Stdin
	r, w, err := os.Pipe()
	require.NoError(t, err)
	_, _ = w.WriteString(payload)
	require.NoError(t, w.Close())
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	err = run(log)
	require.Error(t, err, "run must return error when all retries exhausted")

	data := buf.Bytes()
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	found := false
	for _, ln := range lines {
		var rec map[string]any
		if json.Unmarshal(ln, &rec) == nil && rec["level"] == "ERROR" && rec["msg"] == "hook post exhausted all attempts" {
			require.Equal(t, "hook_post", rec["action"])
			require.Equal(t, "sess-xyz", rec["session_id"])
			require.NotNil(t, rec["duration_ms"])
			require.NotNil(t, rec["attempts"])
			found = true
		}
	}
	require.True(t, found, "no ERROR 'hook post exhausted all attempts' JSON log found")
}
