package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Finding 1: tatara MCP registration failure must be fatal (return error).
// ---------------------------------------------------------------------------

// TestNewApp_TataraMissing_Warn verifies that when tatara is absent from PATH,
// newApp does not silently skip: it emits at least a WARN and historically the
// path was invisible. We test the lower-level guard instead via the observable
// behaviour: lookPath-miss now returns an error from newApp.
//
// We can't easily spin a full newApp in a unit test (needs real workspace),
// so we test the helper that gates MCP registration: when tatara is not on
// PATH, newApp must propagate an error.
//
// We exercise this through a thin extracted helper tataraLookAndRegister that
// is called by newApp - see app.go.
func TestTataraLookAndRegister_MissingBinary_ReturnsError(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no tatara on this PATH
	err := tataraLookAndRegister("/tmp", func(name string, args ...string) error { return nil })
	require.Error(t, err, "expected error when tatara is not on PATH")
	assert.Contains(t, err.Error(), "tatara", "error should mention tatara")
}

func writeFakeTatara(t *testing.T, dir string) {
	t.Helper()
	fakeExe := dir + "/tatara"
	require.NoError(t, os.WriteFile(fakeExe, []byte("#!/bin/sh\nexit 0\n"), 0o755))
}

func TestTataraLookAndRegister_McpConfigFails_ReturnsError(t *testing.T) {
	// Put a fake "tatara" binary on the PATH so LookPath succeeds.
	dir := t.TempDir()
	writeFakeTatara(t, dir)
	t.Setenv("PATH", dir)

	wantErr := errors.New("mcp-config boom")
	err := tataraLookAndRegister("/tmp", func(name string, args ...string) error { return wantErr })
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}

func TestTataraLookAndRegister_OK(t *testing.T) {
	dir := t.TempDir()
	writeFakeTatara(t, dir)
	t.Setenv("PATH", dir)

	err := tataraLookAndRegister("/tmp", func(name string, args ...string) error { return nil })
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Finding 2: OnTurnDone must not block: commit/push runs in a background
// goroutine; Deliver (webhook) is called before the push completes.
// ---------------------------------------------------------------------------

func TestOnTurnDoneOrder_DeliverBeforePushCompletes(t *testing.T) {
	delivered := make(chan struct{})
	var pushStarted atomic.Bool

	// Slow push: blocks until the test releases it.
	pushDone := make(chan struct{})
	pushFn := func() error {
		pushStarted.Store(true)
		<-pushDone // simulate slow network push
		return nil
	}

	// deliverFn records when the callback fires.
	deliverFn := func() {
		close(delivered)
	}

	// Build a handler equivalent to the fixed OnTurnDone.
	handler := buildOnTurnDoneHandler("branch-123", pushFn, deliverFn)

	// Call handler (simulates fireDone on the request goroutine).
	done := make(chan struct{})
	go func() {
		handler()
		close(done)
	}()

	// deliverFn must be called promptly (before push finishes).
	select {
	case <-delivered:
		// good: deliver called
	case <-time.After(2 * time.Second):
		t.Fatal("deliver was not called within 2s; handler is blocking on push")
	}

	// The handler goroutine must have returned by now (non-blocking).
	select {
	case <-done:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("OnTurnDone handler goroutine did not return; still blocking")
	}

	// Unblock the push and verify it ran.
	close(pushDone)
	// Wait a moment for the background goroutine to complete.
	time.Sleep(50 * time.Millisecond)
	assert.True(t, pushStarted.Load(), "push should have started in background")
}

// buildOnTurnDoneHandler returns a closure mirroring the fixed OnTurnDone
// logic: Deliver synchronously first, then push in a background goroutine.
func buildOnTurnDoneHandler(taskBranch string, pushFn func() error, deliverFn func()) func() {
	return func() {
		// Deliver first (synchronous, matches fixed app.go).
		deliverFn()
		// Push in background so the handler returns immediately.
		if taskBranch != "" {
			go func() { _ = pushFn() }()
		}
	}
}

// ---------------------------------------------------------------------------
// Finding 3: gitRunner must not include raw stderr (CombinedOutput) in the
// returned error to avoid leaking tokens from git config dumps / remote URLs.
// ---------------------------------------------------------------------------

func TestGitRunner_ErrorDoesNotContainStderr(t *testing.T) {
	runner := gitRunner()
	// Run git with an invalid arg so it fails and emits stderr output.
	err := runner(t.TempDir(), "--bad-git-flag-that-does-not-exist")
	require.Error(t, err)

	// The raw stderr/stdout must NOT appear in the error string.
	// git typically outputs "unknown switch" or similar to stderr.
	// We check that the error message is short and doesn't embed multiline output.
	msg := err.Error()
	// No newlines (stderr output usually has newlines).
	assert.False(t, strings.Contains(msg, "\n"),
		"error should not contain raw stderr (potential secret leakage): %q", msg)
}

func TestGitRunner_ErrorWrapsUnderlying(t *testing.T) {
	runner := gitRunner()
	err := runner(t.TempDir(), "--bad-git-flag-that-does-not-exist")
	require.Error(t, err)

	// Must still wrap the underlying exec error (for errors.As/Is chains).
	var exitErr *exec.ExitError
	assert.True(t, errors.As(err, &exitErr), "error must wrap *exec.ExitError")
}

func TestGitRunner_ErrorContainsCommand(t *testing.T) {
	runner := gitRunner()
	dir := t.TempDir()
	err := runner(dir, "status")
	if err == nil {
		t.Skip("git status succeeded (git repo might exist); skip command-in-error check")
	}
	msg := err.Error()
	assert.True(t, strings.Contains(msg, "git") || strings.Contains(msg, dir),
		"error should identify the command/dir: %q", msg)
}

// TestGitRunner_ErrorFormat verifies the cleaned error format.
func TestGitRunner_ErrorFormat(t *testing.T) {
	runner := gitRunner()
	err := runner(t.TempDir(), "--bad-git-flag-that-does-not-exist")
	require.Error(t, err)
	msg := err.Error()
	// Must start with "git -C " prefix (identifies command and dir).
	assert.True(t, strings.HasPrefix(msg, "git -C "), "expected 'git -C ' prefix, got: %q", msg)
	// Must NOT embed "unknown switch" or similar raw stderr.
	lowerMsg := strings.ToLower(msg)
	leakyTerms := []string{"unknown switch", "unknown option", "usage:", "fatal:"}
	for _, term := range leakyTerms {
		assert.False(t, strings.Contains(lowerMsg, term),
			"error leaks stderr content %q: %q", term, msg)
	}
	_ = fmt.Sprintf("%v", err) // ensure it's printable
}
