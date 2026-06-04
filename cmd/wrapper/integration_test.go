//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEndToEnd_SubmitTurn_GetResult(t *testing.T) {
	dir := t.TempDir()
	// build stub claude + cc-stop-hook
	stub := filepath.Join(dir, "claude")
	require.NoError(t, exec.Command("go", "build", "-o", stub, "./testdata/stubclaude/main.go").Run())
	hook := filepath.Join(dir, "cc-stop-hook")
	require.NoError(t, exec.Command("go", "build", "-o", hook, "../cc-stop-hook").Run())
	transcript := filepath.Join(dir, "t.jsonl")

	t.Setenv("CLAUDE_PATH", stub)
	t.Setenv("HOOK_PATH", hook)
	t.Setenv("WORKSPACE", dir)
	t.Setenv("HOME_DIR", dir)
	t.Setenv("HTTP_ADDR", "127.0.0.1:18080")
	t.Setenv("INTERNAL_ADDR", "127.0.0.1:18090")
	t.Setenv("STUB_TRANSCRIPT", transcript)
	t.Setenv("STUB_HOOK", hook)
	t.Setenv("CCW_INTERNAL_URL", "http://127.0.0.1:18090/internal/turn-complete")
	t.Setenv("OIDC_ISSUER", "") // skip OIDC

	go func() { _ = run(context.Background(), nil) }()
	requireUp(t, "http://127.0.0.1:18080/readyz")

	body, _ := json.Marshal(map[string]string{"text": "hello"})
	resp, err := http.Post("http://127.0.0.1:18080/v1/messages", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	var pm struct {
		TurnID string `json:"turnId"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&pm))

	require.Eventually(t, func() bool {
		r, err := http.Get("http://127.0.0.1:18080/v1/messages/" + pm.TurnID)
		if err != nil || r.StatusCode != 200 {
			return false
		}
		var rec struct {
			State     string `json:"state"`
			FinalText string `json:"finalText"`
		}
		_ = json.NewDecoder(r.Body).Decode(&rec)
		return rec.State == "complete" && rec.FinalText == "echo:hello"
	}, 10*time.Second, 100*time.Millisecond)
}

func requireUp(t *testing.T, url string) {
	t.Helper()
	require.Eventually(t, func() bool {
		r, err := http.Get(url)
		return err == nil && r.StatusCode == 200
	}, 10*time.Second, 100*time.Millisecond)
}
