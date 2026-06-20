package bootstrap_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
)

func TestRender_WritesClaudeMdSettingsSkillsAndMergesMCP(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	overlay := t.TempDir()
	skillsSrc := t.TempDir()

	// a baked skill source
	require.NoError(t, os.MkdirAll(filepath.Join(skillsSrc, "handoff"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillsSrc, "handoff", "SKILL.md"), []byte("# /handoff"), 0o644))
	// an mcp overlay fragment
	require.NoError(t, os.WriteFile(filepath.Join(overlay, "tasks.json"),
		[]byte(`{"mcpServers":{"tasks":{"type":"stdio","command":"/bin/tasks"}}}`), 0o644))

	var gitCalls [][]string
	p := bootstrap.Params{
		HomeDir: home, Workspace: ws,
		GlobalClaudeMd:  "GLOBAL RULES",
		ProjectClaudeMd: "PROJECT RULES",
		BaseMCP:         []byte(`{"mcpServers":{"tatara-memory":{"type":"stdio","command":"tatara","args":["mcp"]}}}`),
		MCPOverlayDir:   overlay,
		SkillsSrc:       []string{skillsSrc},
		HookCommand:     "/usr/local/bin/cc-stop-hook",
		AllowedTools:    []string{"Bash", "Edit"},
		EnableAllMCP:    true,
		PermissionMode:  "bypassPermissions",
	}
	require.NoError(t, bootstrap.Render(p, func(dir string, a ...string) error { gitCalls = append(gitCalls, a); return nil }))

	// global + project CLAUDE.md
	b, _ := os.ReadFile(filepath.Join(home, ".claude", "CLAUDE.md"))
	require.Contains(t, string(b), "GLOBAL RULES")
	require.Contains(t, string(b), "comment_on_issue") // headless directive appended
	b, _ = os.ReadFile(filepath.Join(ws, "CLAUDE.md"))
	require.Equal(t, "PROJECT RULES", string(b))

	// merged .mcp.json has BOTH servers
	b, _ = os.ReadFile(filepath.Join(ws, ".mcp.json"))
	var mcp struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal(b, &mcp))
	require.Contains(t, mcp.MCPServers, "tatara-memory")
	require.Contains(t, mcp.MCPServers, "tasks")

	// settings.json wires Stop hook + enableAllProjectMcpServers
	b, _ = os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	require.Contains(t, string(b), "/usr/local/bin/cc-stop-hook")
	require.Contains(t, string(b), "enableAllProjectMcpServers")

	// skill copied
	b, _ = os.ReadFile(filepath.Join(ws, ".claude", "skills", "handoff", "SKILL.md"))
	require.Equal(t, "# /handoff", string(b))

	// no repo configured -> git not called
	require.Empty(t, gitCalls)
}

// TestRender_ConsumesOperatorMountedCustomization proves the full subtask-2 ->
// subtask-3 path: the operator mounts project-claude.md, an mcp.d overlay
// fragment, a skill dir, plus the new settings-extra.json and plugins.json, and
// Render consumes ALL of them into the agent's workspace/home in one pass.
func TestRender_ConsumesOperatorMountedCustomization(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	overlay := t.TempDir()   // operator mount: /etc/wrapper/mcp.d
	skillsSrc := t.TempDir() // operator mount: /etc/wrapper/skills

	require.NoError(t, os.MkdirAll(filepath.Join(skillsSrc, "deploy"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillsSrc, "deploy", "SKILL.md"), []byte("# /deploy"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(overlay, "0-github.json"),
		[]byte(`{"mcpServers":{"github":{"type":"stdio","command":"/bin/gh"}}}`), 0o644))

	p := bootstrap.Params{
		HomeDir: home, Workspace: ws,
		ProjectClaudeMd: "PROJECT PROMPT",
		BaseMCP:         []byte(`{"mcpServers":{}}`),
		MCPOverlayDir:   overlay,
		SkillsSrc:       []string{skillsSrc},
		HookCommand:     "/usr/local/bin/cc-stop-hook",
		PermissionMode:  "bypassPermissions",
		Effort:          "xhigh",
		ExtraSettings:   []byte(`{"maxParallelism":3}`),
		Plugins:         []bootstrap.PluginSpec{{Name: "fmt@acme", Source: "acme/plugins"}},
	}
	require.NoError(t, bootstrap.Render(p, func(dir string, a ...string) error { return nil }))

	// systemPrompt -> workspace CLAUDE.md
	b, _ := os.ReadFile(filepath.Join(ws, "CLAUDE.md"))
	require.Equal(t, "PROJECT PROMPT", string(b))
	// mcp overlay fragment merged
	b, _ = os.ReadFile(filepath.Join(ws, ".mcp.json"))
	require.Contains(t, string(b), "github")
	// skill copied
	b, _ = os.ReadFile(filepath.Join(ws, ".claude", "skills", "deploy", "SKILL.md"))
	require.Equal(t, "# /deploy", string(b))
	// settings carry extra passthrough, effort, and declarative plugins
	b, _ = os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	var s map[string]any
	require.NoError(t, json.Unmarshal(b, &s))
	require.EqualValues(t, 3, s["maxParallelism"])
	require.Equal(t, "xhigh", s["effortLevel"])
	require.Contains(t, s["enabledPlugins"].(map[string]any), "fmt@acme")
	require.Contains(t, s["extraKnownMarketplaces"].(map[string]any), "acme")
}

func TestRender_ClonesRepoWhenURLSet(t *testing.T) {
	var gitCalls [][]string
	p := bootstrap.Params{
		HomeDir: t.TempDir(), Workspace: t.TempDir(),
		BaseMCP: []byte(`{"mcpServers":{}}`),
		RepoURL: "https://github.com/x/y", RepoBranch: "main",
		HookCommand: "/usr/local/bin/cc-stop-hook", PermissionMode: "bypassPermissions",
	}
	require.NoError(t, bootstrap.Render(p, func(dir string, a ...string) error { gitCalls = append(gitCalls, a); return nil }))
	require.Len(t, gitCalls, 1)
	require.Contains(t, gitCalls[0], "clone")
	require.Contains(t, gitCalls[0], "https://github.com/x/y")
	require.Contains(t, gitCalls[0], "main")
}

// TestRender_MultiRepo_SkipsEmptyNamespacePath asserts that a repo whose URL
// yields an empty namespacePath (empty string or single-segment) is never
// cloned into the workspace root. For a non-primary repo it must be silently
// skipped; for a primary repo Render must return a clear error.
func TestRender_MultiRepo_SkipsEmptyNamespacePath(t *testing.T) {
	ws := t.TempDir()

	var cloneDests []string
	fakeGit := func(dir string, a ...string) error {
		// record the destination argument of every clone call
		for i, arg := range a {
			if arg == "clone" && i+3 < len(a) {
				// args: clone [--depth 1] [--branch b] <url> <dest>
				cloneDests = append(cloneDests, a[len(a)-1])
			}
		}
		return nil
	}

	p := bootstrap.Params{
		HomeDir:        t.TempDir(),
		Workspace:      ws,
		BaseMCP:        []byte(`{"mcpServers":{}}`),
		HookCommand:    "/usr/local/bin/cc-stop-hook",
		PermissionMode: "bypassPermissions",
		// Primary repo has a valid URL.
		RepoURL:    "https://github.com/owner/primary.git",
		RepoBranch: "main",
		Repos: []bootstrap.RepoSpec{
			{Name: "primary", URL: "https://github.com/owner/primary.git", Branch: "main"},
			// empty URL -> namespacePath returns "" -> dest would equal workspace root
			{Name: "bad-empty", URL: "", Branch: "main"},
			// single-segment URL -> namespacePath returns "repo" with no slash -> still
			// resolves to a subdir, but there is no owner segment; test the "" case only
			// for clarity; the single-segment case is an edge-case variant tested below.
		},
	}

	// Non-primary bad repo must be skipped, not cause an error.
	require.NoError(t, bootstrap.Render(p, fakeGit))

	// The workspace root itself must never appear as a clone destination.
	for _, dest := range cloneDests {
		require.NotEqual(t, ws, dest, "clone must not target workspace root (dest=%q)", dest)
		// Also reject any filepath.Clean that resolves to ws.
		require.NotEqual(t, ws, filepath.Clean(dest), "clean dest must not equal workspace (dest=%q)", dest)
	}
}

// TestRender_MultiRepo_PrimaryEmptyURLReturnsError asserts that when the
// primary repo (r.URL == p.RepoURL) has an empty URL that would resolve to the
// workspace root, Render returns a descriptive error instead of cloning there.
func TestRender_MultiRepo_PrimaryEmptyURLReturnsError(t *testing.T) {
	ws := t.TempDir()
	fakeGit := func(dir string, a ...string) error { return nil }

	p := bootstrap.Params{
		HomeDir:        t.TempDir(),
		Workspace:      ws,
		BaseMCP:        []byte(`{"mcpServers":{}}`),
		HookCommand:    "/usr/local/bin/cc-stop-hook",
		PermissionMode: "bypassPermissions",
		RepoURL:        "",
		Repos: []bootstrap.RepoSpec{
			// Primary with empty URL.
			{Name: "bad-primary", URL: "", Branch: "main"},
		},
	}

	err := bootstrap.Render(p, fakeGit)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot derive namespace path from URL")
}

// scriptedGit is a recording fake GitRunner. For each call it matches the
// args against its scripts in order (first match wins) and returns the
// scripted error. All calls are recorded in Calls regardless of the result.
type scriptedGit struct {
	// scripts is a list of (predicate, error) pairs evaluated in order.
	scripts []struct {
		match func(args []string) bool
		err   error
	}
	Calls [][]string
}

func (s *scriptedGit) run(dir string, args ...string) error {
	s.Calls = append(s.Calls, args)
	for _, sc := range s.scripts {
		if sc.match(args) {
			return sc.err
		}
	}
	return nil
}

func argsContainAll(needles ...string) func([]string) bool {
	return func(args []string) bool {
		for _, n := range needles {
			found := false
			for _, a := range args {
				if a == n {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
}

func callsContainingAll(calls [][]string, needles ...string) [][]string {
	var out [][]string
	for _, c := range calls {
		match := true
		for _, n := range needles {
			found := false
			for _, a := range c {
				if a == n {
					found = true
					break
				}
			}
			if !found {
				match = false
				break
			}
		}
		if match {
			out = append(out, c)
		}
	}
	return out
}

// TestCheckoutTaskBranch_BranchExists_ResumesFromRemote asserts that when
// ls-remote reports the task branch exists on origin, Render fetches
// --unshallow, fetches the branch, and checks out using -B <branch> FETCH_HEAD
// (resume path). It must NOT issue a plain "checkout -b <branch>".
func TestCheckoutTaskBranch_BranchExists_ResumesFromRemote(t *testing.T) {
	sg := &scriptedGit{}
	taskBranch := "tatara/task-abc123"
	// ls-remote --exit-code --heads origin <branch> -> nil means branch exists
	sg.scripts = append(sg.scripts, struct {
		match func([]string) bool
		err   error
	}{argsContainAll("ls-remote", "--exit-code"), nil})

	p := bootstrap.Params{
		HomeDir: t.TempDir(), Workspace: t.TempDir(),
		BaseMCP:        []byte(`{"mcpServers":{}}`),
		RepoURL:        "https://github.com/x/y",
		RepoBranch:     "main",
		TaskBranch:     taskBranch,
		HookCommand:    "/usr/local/bin/cc-stop-hook",
		PermissionMode: "bypassPermissions",
	}
	require.NoError(t, bootstrap.Render(p, sg.run))

	// Must have called fetch --unshallow
	unshallow := callsContainingAll(sg.Calls, "fetch", "--unshallow")
	require.NotEmpty(t, unshallow, "expected fetch --unshallow to be called on resume path")

	// Must have called fetch origin <taskBranch>
	fetchBranch := callsContainingAll(sg.Calls, "fetch", "origin", taskBranch)
	require.NotEmpty(t, fetchBranch, "expected fetch origin <taskBranch> to be called on resume path")

	// Must have called checkout -B <taskBranch> FETCH_HEAD
	checkoutResume := callsContainingAll(sg.Calls, "checkout", "-B", taskBranch, "FETCH_HEAD")
	require.NotEmpty(t, checkoutResume, "expected checkout -B <branch> FETCH_HEAD to be called on resume path")

	// Must NOT have called plain checkout -b <taskBranch> (fresh-branch path)
	freshCheckout := callsContainingAll(sg.Calls, "checkout", "-b", taskBranch)
	require.Empty(t, freshCheckout, "checkout -b must NOT be called when branch already exists on remote")
}

// TestCheckoutTaskBranch_BranchAbsent_FreshBranch asserts that when ls-remote
// returns a non-nil error (branch not found), Render issues the plain
// "checkout -b <branch>" and does NOT fetch --unshallow or fetch origin <branch>.
func TestCheckoutTaskBranch_BranchAbsent_FreshBranch(t *testing.T) {
	taskBranch := "tatara/task-newbranch"
	sg := &scriptedGit{}
	// ls-remote --exit-code -> non-nil error means branch does NOT exist
	sg.scripts = append(sg.scripts, struct {
		match func([]string) bool
		err   error
	}{argsContainAll("ls-remote", "--exit-code"), fmt.Errorf("exit status 2")})

	p := bootstrap.Params{
		HomeDir: t.TempDir(), Workspace: t.TempDir(),
		BaseMCP:        []byte(`{"mcpServers":{}}`),
		RepoURL:        "https://github.com/x/y",
		RepoBranch:     "main",
		TaskBranch:     taskBranch,
		HookCommand:    "/usr/local/bin/cc-stop-hook",
		PermissionMode: "bypassPermissions",
	}
	require.NoError(t, bootstrap.Render(p, sg.run))

	// Must have called plain checkout -b <taskBranch>
	freshCheckout := callsContainingAll(sg.Calls, "checkout", "-b", taskBranch)
	require.NotEmpty(t, freshCheckout, "expected checkout -b <branch> on fresh-task path")

	// Must NOT have called fetch --unshallow
	unshallow := callsContainingAll(sg.Calls, "fetch", "--unshallow")
	require.Empty(t, unshallow, "fetch --unshallow must NOT be called on fresh-task path")

	// Must NOT have called fetch origin <taskBranch>
	fetchBranch := callsContainingAll(sg.Calls, "fetch", "origin", taskBranch)
	require.Empty(t, fetchBranch, "fetch origin <branch> must NOT be called on fresh-task path")
}

// TestCheckoutTaskBranch_UnshallowErrorPropagates asserts that a fetch
// --unshallow failure aborts the resume (returns an error) rather than
// proceeding with a shallow clone whose rebase would fail with no merge base.
// The clone is always --depth 1, so an unshallow failure is a real (network)
// error, never the benign "already complete" case.
func TestCheckoutTaskBranch_UnshallowErrorPropagates(t *testing.T) {
	taskBranch := "tatara/task-exists"
	sg := &scriptedGit{}
	// ls-remote -> nil (branch exists)
	sg.scripts = append(sg.scripts, struct {
		match func([]string) bool
		err   error
	}{argsContainAll("ls-remote", "--exit-code"), nil})
	// fetch --unshallow -> error (genuine failure)
	sg.scripts = append(sg.scripts, struct {
		match func([]string) bool
		err   error
	}{argsContainAll("fetch", "--unshallow"), fmt.Errorf("unshallow: network unreachable")})

	p := bootstrap.Params{
		HomeDir: t.TempDir(), Workspace: t.TempDir(),
		BaseMCP:        []byte(`{"mcpServers":{}}`),
		RepoURL:        "https://github.com/x/y",
		RepoBranch:     "main",
		TaskBranch:     taskBranch,
		HookCommand:    "/usr/local/bin/cc-stop-hook",
		PermissionMode: "bypassPermissions",
	}
	// Render must surface the unshallow failure, not swallow it.
	err := bootstrap.Render(p, sg.run)
	require.Error(t, err, "unshallow failure on the resume path must abort Render")
	require.Contains(t, err.Error(), "unshallow")

	// Must NOT have proceeded to fetch the branch or checkout once unshallow failed.
	require.Empty(t, callsContainingAll(sg.Calls, "checkout", "-B", taskBranch, "FETCH_HEAD"),
		"must not checkout the resumed branch after an unshallow failure")
}

// TestRender_LogsAndMetricsOnClone verifies that Render emits an INFO log for
// each successful clone/resume and increments BootstrapCloneTotal{result=ok}
// and observes BootstrapDuration (finding 2).
func TestRender_LogsAndMetricsOnClone(t *testing.T) {
	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)

	p := bootstrap.Params{
		HomeDir: t.TempDir(), Workspace: t.TempDir(),
		BaseMCP:        []byte(`{"mcpServers":{}}`),
		RepoURL:        "https://github.com/x/y",
		RepoBranch:     "main",
		HookCommand:    "/usr/local/bin/cc-stop-hook",
		PermissionMode: "bypassPermissions",
		Log:            log,
		M:              m,
	}
	// fake git: ls-remote exits non-nil (fresh task, no task branch)
	require.NoError(t, bootstrap.Render(p, func(dir string, a ...string) error { return nil }))

	// Check INFO log emitted
	data := logBuf.Bytes()
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	found := false
	for _, ln := range lines {
		var rec map[string]any
		if json.Unmarshal(ln, &rec) == nil && rec["msg"] == "repo cloned" {
			require.Equal(t, "clone", rec["action"])
			require.NotNil(t, rec["duration_ms"])
			found = true
		}
	}
	require.True(t, found, "no 'repo cloned' INFO log from Render")

	// Check metrics
	mfs, err := reg.Gather()
	require.NoError(t, err)
	byName := map[string]bool{}
	for _, mf := range mfs {
		byName[mf.GetName()] = true
		switch mf.GetName() {
		case "ccw_bootstrap_clone_total":
			for _, mm := range mf.GetMetric() {
				for _, lp := range mm.GetLabel() {
					if lp.GetName() == "result" && lp.GetValue() == "ok" {
						require.Greater(t, mm.GetCounter().GetValue(), float64(0))
					}
				}
			}
		case "ccw_bootstrap_duration_seconds":
			hist := mf.GetMetric()[0].GetHistogram()
			require.Greater(t, hist.GetSampleCount(), uint64(0), "BootstrapDuration not observed")
		}
	}
	require.True(t, byName["ccw_bootstrap_clone_total"], "ccw_bootstrap_clone_total not registered")
	require.True(t, byName["ccw_bootstrap_duration_seconds"], "ccw_bootstrap_duration_seconds not registered")
}

// TestRender_WithoutLogAndMetrics verifies that nil Log/M doesn't panic (finding 2 backward compat).
func TestRender_WithoutLogAndMetrics(t *testing.T) {
	p := bootstrap.Params{
		HomeDir: t.TempDir(), Workspace: t.TempDir(),
		BaseMCP:        []byte(`{"mcpServers":{}}`),
		HookCommand:    "/usr/local/bin/cc-stop-hook",
		PermissionMode: "bypassPermissions",
		// Log and M intentionally nil
	}
	require.NoError(t, bootstrap.Render(p, func(dir string, a ...string) error { return nil }))
}

// TestRender_MCPJsonPermissions verifies that .mcp.json is written 0644
// (non-secret config) rather than 0600 (finding 7).
func TestRender_MCPJsonPermissions(t *testing.T) {
	ws := t.TempDir()
	p := bootstrap.Params{
		HomeDir:        t.TempDir(),
		Workspace:      ws,
		BaseMCP:        []byte(`{"mcpServers":{}}`),
		HookCommand:    "/usr/local/bin/cc-stop-hook",
		PermissionMode: "bypassPermissions",
	}
	require.NoError(t, bootstrap.Render(p, func(dir string, a ...string) error { return nil }))
	info, err := os.Stat(filepath.Join(ws, ".mcp.json"))
	require.NoError(t, err)
	got := info.Mode().Perm()
	require.Equal(t, os.FileMode(0o644), got, ".mcp.json must be 0644, got %o", got)
}

// TestRender_MultiRepo_PrimaryIdentifiedByPosition verifies that a clone failure
// of the first repo (index 0) is escalated even when p.RepoURL is empty
// (finding 2: primary by position, not URL match).
func TestRender_MultiRepo_PrimaryIdentifiedByPosition(t *testing.T) {
	fakeGit := func(dir string, args ...string) error {
		// Fail all clone calls to trigger the primary escalation path.
		for _, a := range args {
			if a == "clone" {
				return fmt.Errorf("clone failed: network unreachable")
			}
		}
		return nil
	}

	p := bootstrap.Params{
		HomeDir:        t.TempDir(),
		Workspace:      t.TempDir(),
		BaseMCP:        []byte(`{"mcpServers":{}}`),
		HookCommand:    "/usr/local/bin/cc-stop-hook",
		PermissionMode: "bypassPermissions",
		RepoURL:        "", // intentionally empty
		Repos: []bootstrap.RepoSpec{
			{Name: "primary", URL: "https://github.com/owner/primary.git", Branch: "main"},
			{Name: "secondary", URL: "https://github.com/owner/secondary.git", Branch: "main"},
		},
	}

	err := bootstrap.Render(p, fakeGit)
	require.Error(t, err, "clone failure of Repos[0] must return an error even when p.RepoURL is empty")
	require.Contains(t, err.Error(), "primary")
}

var _ = io.Discard // keep io import if unused

func TestRender_ConfiguresGitCredentialsAndIdentityBeforeClone(t *testing.T) {
	var gitCalls [][]string
	p := bootstrap.Params{
		HomeDir: t.TempDir(), Workspace: t.TempDir(),
		BaseMCP:      []byte(`{"mcpServers":{}}`),
		RepoURL:      "https://github.com/x/y",
		RepoBranch:   "main",
		GitToken:     "ghp_supersecret",
		GitUserName:  "tatara-agent",
		GitUserEmail: "tatara-agent@szymonrichert.pl",
		HookCommand:  "/usr/local/bin/cc-stop-hook", PermissionMode: "bypassPermissions",
	}
	require.NoError(t, bootstrap.Render(p, func(dir string, a ...string) error { gitCalls = append(gitCalls, a); return nil }))

	var credIdx, nameIdx, emailIdx, cloneIdx = -1, -1, -1, -1
	for i, c := range gitCalls {
		j := strings.Join(c, " ")
		switch {
		case strings.Contains(j, "credential.helper"):
			credIdx = i
			// helper reads the token from the env, never embeds it literally.
			require.Contains(t, j, "GIT_TOKEN")
			require.NotContains(t, j, "ghp_supersecret")
		case strings.Contains(j, "user.name"):
			nameIdx = i
		case strings.Contains(j, "user.email"):
			emailIdx = i
		case strings.Contains(j, "clone"):
			cloneIdx = i
		}
	}
	require.GreaterOrEqual(t, credIdx, 0, "credential.helper not configured")
	require.GreaterOrEqual(t, nameIdx, 0, "user.name not configured")
	require.GreaterOrEqual(t, emailIdx, 0, "user.email not configured")
	require.GreaterOrEqual(t, cloneIdx, 0, "repo not cloned")
	require.Less(t, credIdx, cloneIdx, "credentials must be set before clone")
}

// TestRender_ResumeDoesNotDuplicateLsRemote asserts that when a task branch
// already exists on the remote, Render calls ls-remote exactly ONCE, not twice
// (finding 2: checkoutTaskBranch called it, then Render called it again to set
// action="resume", doubling the network round-trip).
func TestRender_ResumeDoesNotDuplicateLsRemote(t *testing.T) {
	taskBranch := "tatara/task-dedup"
	sg := &scriptedGit{}
	// ls-remote -> nil (branch exists on remote) for all calls
	sg.scripts = append(sg.scripts, struct {
		match func([]string) bool
		err   error
	}{argsContainAll("ls-remote", "--exit-code"), nil})

	p := bootstrap.Params{
		HomeDir: t.TempDir(), Workspace: t.TempDir(),
		BaseMCP:        []byte(`{"mcpServers":{}}`),
		RepoURL:        "https://github.com/x/y",
		RepoBranch:     "main",
		TaskBranch:     taskBranch,
		HookCommand:    "/usr/local/bin/cc-stop-hook",
		PermissionMode: "bypassPermissions",
	}
	require.NoError(t, bootstrap.Render(p, sg.run))

	lsRemoteCalls := callsContainingAll(sg.Calls, "ls-remote", "--exit-code")
	require.Equal(t, 1, len(lsRemoteCalls),
		"ls-remote must be called exactly once per repo (no duplicate after checkoutTaskBranch); got %d calls", len(lsRemoteCalls))
}

// TestRender_MultiRepo_ResumeDoesNotDuplicateLsRemote asserts the same
// dedup property in multi-repo mode (two repos, each ls-remote called once).
func TestRender_MultiRepo_ResumeDoesNotDuplicateLsRemote(t *testing.T) {
	taskBranch := "tatara/task-multidedup"
	sg := &scriptedGit{}
	sg.scripts = append(sg.scripts, struct {
		match func([]string) bool
		err   error
	}{argsContainAll("ls-remote", "--exit-code"), nil})

	p := bootstrap.Params{
		HomeDir:        t.TempDir(),
		Workspace:      t.TempDir(),
		BaseMCP:        []byte(`{"mcpServers":{}}`),
		HookCommand:    "/usr/local/bin/cc-stop-hook",
		PermissionMode: "bypassPermissions",
		TaskBranch:     taskBranch,
		Repos: []bootstrap.RepoSpec{
			{Name: "a", URL: "https://github.com/owner/repo-a", Branch: "main"},
			{Name: "b", URL: "https://github.com/owner/repo-b", Branch: "main"},
		},
	}
	require.NoError(t, bootstrap.Render(p, sg.run))

	lsRemoteCalls := callsContainingAll(sg.Calls, "ls-remote", "--exit-code")
	require.Equal(t, 2, len(lsRemoteCalls),
		"ls-remote must be called exactly once per repo (2 repos = 2 calls); got %d", len(lsRemoteCalls))
}

// TestRender_MultiRepo_SameOwnerRepoDifferentHostsAreDetected asserts that
// when two distinct repo URLs map to the same namespace path (same owner/repo
// on different hosts), Render returns an error rather than silently skipping
// the second clone (finding 4).
func TestRender_MultiRepo_SameOwnerRepoDifferentHostsCollisionDetected(t *testing.T) {
	fakeGit := func(dir string, args ...string) error { return nil }

	p := bootstrap.Params{
		HomeDir:        t.TempDir(),
		Workspace:      t.TempDir(),
		BaseMCP:        []byte(`{"mcpServers":{}}`),
		HookCommand:    "/usr/local/bin/cc-stop-hook",
		PermissionMode: "bypassPermissions",
		Repos: []bootstrap.RepoSpec{
			{Name: "github-repo", URL: "https://github.com/owner/repo", Branch: "main"},
			// Same owner/repo on a different host -> same namespace path
			{Name: "gitlab-repo", URL: "https://gitlab.com/owner/repo", Branch: "main"},
		},
	}

	err := bootstrap.Render(p, fakeGit)
	require.Error(t, err, "same namespace path from different hosts must cause an error")
	require.Contains(t, err.Error(), "collision")
}

// TestRender_SingleRepo_ClonesIntoNamespaceSubdir verifies that in single-repo
// mode (RepoURL set, Repos empty) the repo is cloned into a namespace subdir
// (workspace/owner/repo) rather than directly into workspace, so that injected
// session files (CLAUDE.md, .mcp.json) at the workspace root are never inside
// the repo's working tree and cannot be committed into the PR (finding 1).
func TestRender_SingleRepo_ClonesIntoNamespaceSubdir(t *testing.T) {
	ws := t.TempDir()
	var cloneDest string
	fakeGit := func(dir string, args ...string) error {
		for i, a := range args {
			if a == "clone" && i+2 < len(args) {
				cloneDest = args[len(args)-1]
			}
		}
		return nil
	}
	p := bootstrap.Params{
		HomeDir:         t.TempDir(),
		Workspace:       ws,
		BaseMCP:         []byte(`{"mcpServers":{}}`),
		ProjectClaudeMd: "PROJECT RULES",
		RepoURL:         "https://github.com/owner/myrepo.git",
		RepoBranch:      "main",
		HookCommand:     "/usr/local/bin/cc-stop-hook",
		PermissionMode:  "bypassPermissions",
	}
	require.NoError(t, bootstrap.Render(p, fakeGit))

	// Clone must target the namespace subdir, not the workspace root.
	wantDest := filepath.Join(ws, "owner", "myrepo")
	require.Equal(t, wantDest, cloneDest,
		"single-repo clone must target namespace subdir, not workspace root")

	// Session config stays at workspace root (never inside the repo subdir).
	require.FileExists(t, filepath.Join(ws, "CLAUDE.md"))
	require.FileExists(t, filepath.Join(ws, ".mcp.json"))
	require.NoFileExists(t, filepath.Join(wantDest, "CLAUDE.md"),
		"CLAUDE.md must not be inside the repo subdir (would pollute PR diff)")
	require.NoFileExists(t, filepath.Join(wantDest, ".mcp.json"),
		".mcp.json must not be inside the repo subdir (would pollute PR diff)")
}

// TestRender_EmitsBootstrapRenderLog verifies that a successful config-render phase
// emits an INFO log with action=bootstrap_render and duration_ms, and increments
// BootstrapRenderTotal{result=ok} (finding 5).
func TestRender_EmitsBootstrapRenderLog(t *testing.T) {
	var logBuf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)

	p := bootstrap.Params{
		HomeDir:        t.TempDir(),
		Workspace:      t.TempDir(),
		BaseMCP:        []byte(`{"mcpServers":{}}`),
		HookCommand:    "/usr/local/bin/cc-stop-hook",
		PermissionMode: "bypassPermissions",
		Log:            log,
		M:              m,
	}
	require.NoError(t, bootstrap.Render(p, func(dir string, a ...string) error { return nil }))

	// Verify INFO log with action=bootstrap_render and duration_ms.
	data := logBuf.Bytes()
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	found := false
	for _, ln := range lines {
		var rec map[string]any
		if json.Unmarshal(ln, &rec) == nil && rec["msg"] == "bootstrap config rendered" {
			require.Equal(t, "bootstrap_render", rec["action"], "action field must be bootstrap_render")
			require.NotNil(t, rec["duration_ms"], "duration_ms must be present")
			found = true
		}
	}
	require.True(t, found, "no 'bootstrap config rendered' INFO log from Render")

	// Verify BootstrapRenderTotal{result=ok} incremented.
	mfs, err := reg.Gather()
	require.NoError(t, err)
	var renderOK float64
	for _, mf := range mfs {
		if mf.GetName() == "ccw_bootstrap_render_total" {
			for _, mm := range mf.GetMetric() {
				for _, lp := range mm.GetLabel() {
					if lp.GetName() == "result" && lp.GetValue() == "ok" {
						renderOK = mm.GetCounter().GetValue()
					}
				}
			}
		}
	}
	require.Equal(t, float64(1), renderOK, "BootstrapRenderTotal{result=ok} must be 1 after successful render")
}
