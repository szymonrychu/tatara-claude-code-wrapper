package main

import (
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
