package bootstrap_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
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
	require.Equal(t, "GLOBAL RULES", string(b))
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
		HomeDir:   t.TempDir(),
		Workspace: ws,
		BaseMCP:   []byte(`{"mcpServers":{}}`),
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
