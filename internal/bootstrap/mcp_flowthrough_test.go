package bootstrap_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/promptguard"
)

// tataraBinary returns the path to the baked tatara CLI, preferring the
// Dockerfile install location, falling back to PATH. Empty string => not found.
func tataraBinary() string {
	const baked = "/usr/local/bin/tatara"
	if _, err := exec.LookPath(baked); err == nil {
		return baked
	}
	if p, err := exec.LookPath("tatara"); err == nil {
		return p
	}
	if abs, err := filepath.Abs(baked); err == nil {
		if _, err := exec.LookPath(abs); err == nil {
			return abs
		}
	}
	return ""
}

type rpcReq struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type toolsListResult struct {
	Result struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	} `json:"result"`
}

// TestTataraMCP_AdvertisesScmProjectTools proves the baked tatara CLI's `mcp`
// server advertises the tool surface the wrapper relies on flowing through
// automatically (RegisterTataraMCP runs `tatara mcp-config`, which wires
// `tatara mcp`; the wrapper never enumerates tools, so this is the only guard
// against a silent regression when the baked cli version is bumped).
//
// tools/list is per-profile (cli contract D.6: resolveProfile fails CLOSED to
// six always-on tools when TATARA_TOOL_PROFILE is unset). In production the
// operator sets that env var per pod kind; here we set it to "refine", the one
// profile whose grant set covers both issue_write and mr_write alongside the
// always-on six and submit_outcome, so a single run exercises the full surface
// this guard cares about.
func TestTataraMCP_AdvertisesScmProjectTools(t *testing.T) {
	names := liveToolNames(t, "refine")
	for _, want := range []string{
		"submit_outcome", "scm_read", "issue_write", "mr_write",
		"task_get", "project_get", "task_context", "task_note", "report_internal_issue",
	} {
		require.Containsf(t, names, want, "tatara mcp must advertise %q; got %v", want, names)
	}
	for _, gone := range []string{
		"propose_issue", "review_verdict", "pr_outcome", "issue_outcome",
		"change_summary", "comment", "create_subtask",
	} {
		require.NotContainsf(t, names, gone, "tatara mcp must NOT advertise %q (removed in the redesign); got %v", gone, names)
	}
}

// requireLiveGuardEnv is set to "required" by the Dockerfile guard stage, where
// the tatara binary is baked and a skip would silently retire the guard. Set
// there and nowhere else: on a developer laptop (and in the plain `go test`
// CI job) there is no baked cli, so these tests skip.
const requireLiveGuardEnv = "TATARA_MCP_GUARD"

// liveToolNames drives a real `tatara mcp` over stdio and returns the tool names
// it advertises for the given profile. It skips the test when no tatara binary
// is present - unless TATARA_MCP_GUARD=required, in which case a missing binary
// is a hard failure. Without that, the image build's guard stage could lose the
// binary and go green on a skip, which is the fail-open shape this guard exists
// to rule out.
func liveToolNames(t *testing.T, profile string) []string {
	t.Helper()
	bin := tataraBinary()
	if bin == "" {
		if os.Getenv(requireLiveGuardEnv) == "required" {
			t.Fatalf("%s=required but no tatara binary was found: the live MCP tool guard must run, not skip", requireLiveGuardEnv)
		}
		t.Skip("tatara binary not found; runs in the image stage / CI where it is baked")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	cmd := exec.CommandContext(ctx, bin, "mcp") //nolint:gosec // bin is resolved via exec.LookPath, not user input
	cmd.Env = append(os.Environ(), "TATARA_TOOL_PROFILE="+profile)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})

	send := func(r rpcReq) {
		b, err := json.Marshal(r)
		require.NoError(t, err)
		_, err = stdin.Write(append(b, '\n'))
		require.NoError(t, err)
	}

	send(rpcReq{JSONRPC: "2.0", ID: 1, Method: "initialize", Params: map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "wrapper-flowthrough-test", "version": "1"},
	}})
	send(rpcReq{JSONRPC: "2.0", ID: 2, Method: "tools/list"})

	return collectToolNames(t, stdout)
}

// TestAgentPromptToolNamesAreAdvertised is the regression guard for #136: every
// MCP tool name this repo puts in front of an agent - today the injected global
// CLAUDE.md, tomorrow whatever prompt constant someone adds next - must still be
// advertised by the baked cli. The names are discovered by scanning this repo's
// own prompt text (internal/promptguard), not read from a hand-kept list, so a
// name added to prose without being registered anywhere is still checked.
//
// This is what was missing on 2026-07-13: tatara-cli reaped comment_on_issue and
// decline_implementation, the injected CLAUDE.md kept naming both for 14 days,
// and every image built green because nothing compared the two.
//
// Profile "refine" matches the sibling guard above: it is the one profile whose
// grants cover issue_write and mr_write alongside the always-on set, so a single
// run can see every tool the wrapper's own prompt text may name.
func TestAgentPromptToolNamesAreAdvertised(t *testing.T) {
	root, err := promptguard.RepoRoot()
	require.NoError(t, err)
	lits, err := promptguard.Literals(root)
	require.NoError(t, err)
	referenced := promptguard.ToolNames(lits)
	require.NotEmpty(t, referenced,
		"the prompt scan found no MCP tool names at all; the guard would pass vacuously")

	names := liveToolNames(t, "refine")
	for _, tool := range referenced {
		require.Containsf(t, names, tool,
			"agent-facing prompt text at %s names the MCP tool %q, which the baked tatara cli does not advertise (%v). "+
				"Either the tool was renamed or retired - fix the prompt text, do not relax this guard.",
			promptguard.Where(lits, tool), tool, names)
	}
}

// collectToolNames reads newline-delimited JSON-RPC responses until it sees the
// tools/list result (the one carrying a non-empty tools array), returning the
// advertised tool names.
func collectToolNames(t *testing.T, r io.Reader) []string {
	t.Helper()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var res toolsListResult
		if err := json.Unmarshal(line, &res); err != nil {
			continue
		}
		if len(res.Result.Tools) == 0 {
			continue
		}
		names := make([]string, 0, len(res.Result.Tools))
		for _, tl := range res.Result.Tools {
			names = append(names, tl.Name)
		}
		return names
	}
	require.NoError(t, sc.Err())
	t.Fatal("tatara mcp produced no tools/list result")
	return nil
}
