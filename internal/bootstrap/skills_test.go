package bootstrap

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
)

// ---- frontmatter parser tests ----

func TestParseProfiles_InlineList(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "wildcard inline",
			input:    "---\nname: foo\nprofiles: [\"*\"]\n---\n# body",
			expected: []string{"*"},
		},
		{
			name:     "multiple profiles inline",
			input:    "---\nname: foo\nprofiles: [\"implement\", \"brainstorm\"]\n---\n# body",
			expected: []string{"implement", "brainstorm"},
		},
		{
			name:     "single profile inline",
			input:    "---\nname: foo\nprofiles: [review]\n---\n",
			expected: []string{"review"},
		},
		{
			name:     "empty inline list",
			input:    "---\nname: foo\nprofiles: []\n---\n",
			expected: []string{},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := parseProfiles([]byte(tc.input))
			require.Equal(t, tc.expected, got)
		})
	}
}

func TestParseProfiles_BlockList(t *testing.T) {
	input := "---\nname: foo\nprofiles:\n  - implement\n  - lifecycle\n---\n# body"
	got := parseProfiles([]byte(input))
	require.Equal(t, []string{"implement", "lifecycle"}, got)
}

func TestParseProfiles_MissingField_ReturnsNil(t *testing.T) {
	input := "---\nname: foo\ndescription: bar\n---\n# body"
	got := parseProfiles([]byte(input))
	require.Nil(t, got)
}

func TestParseProfiles_NoFrontmatter_ReturnsNil(t *testing.T) {
	got := parseProfiles([]byte("# just a comment\n"))
	require.Nil(t, got)
}

func TestParseProfiles_UnterminatedFrontmatter_ReturnsNil(t *testing.T) {
	got := parseProfiles([]byte("---\nname: foo\n"))
	require.Nil(t, got)
}

// TestParseProfiles_BlockScalarNoFalseMatch asserts that a "profiles:" key
// indented inside a description: > block scalar is NOT treated as the profiles
// field; only the top-level profiles key is returned.
func TestParseProfiles_BlockScalarNoFalseMatch(t *testing.T) {
	// "profiles: [\"fake\"]" is indented under "description: >" - must be ignored.
	// The real "profiles: [\"brainstorm\"]" sits at column 0 after the block.
	input := "---\nname: foo\ndescription: >\n  profiles: [\"fake\"]\nprofiles: [\"brainstorm\"]\n---\n# body"
	got := parseProfiles([]byte(input))
	require.Equal(t, []string{"brainstorm"}, got)
}

// TestParseProfiles_CRLF asserts that files with CRLF line endings are
// correctly parsed (filtering stays active, profiles is not nil).
func TestParseProfiles_CRLF(t *testing.T) {
	input := "---\r\nname: foo\r\nprofiles: [\"review\"]\r\n---\r\n# body"
	got := parseProfiles([]byte(input))
	require.Equal(t, []string{"review"}, got)
}

// ---- filter decision tests ----

func TestShouldInstall_Matrix(t *testing.T) {
	cases := []struct {
		name          string
		activeProfile string
		profiles      []string
		want          bool
	}{
		{"empty profile installs all", "", []string{"implement"}, true},
		{"empty profile with nil profiles", "", nil, true},
		{"wildcard always installs", "review", []string{"*"}, true},
		{"nil profiles treated as wildcard", "implement", nil, true},
		{"empty slice treated as wildcard", "implement", []string{}, true},
		{"exact match installs", "brainstorm", []string{"brainstorm", "incident"}, true},
		{"non-match skips", "review", []string{"implement", "brainstorm"}, false},
		{"triage match", "triage", []string{"triage", "lifecycle"}, true},
		{"incident not in implement profile", "implement", []string{"incident"}, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := shouldInstall(tc.activeProfile, tc.profiles)
			require.Equal(t, tc.want, got)
		})
	}
}

// ---- installSkills end-to-end filter tests ----

func TestInstallSkills_FiltersByProfile(t *testing.T) {
	src := t.TempDir()
	// skill-a: profiles ["implement"] - should be installed
	mustMkdir(t, filepath.Join(src, "skill-a"))
	mustWriteFile(t, filepath.Join(src, "skill-a", "SKILL.md"),
		"---\nname: skill-a\nprofiles: [\"implement\"]\n---\n# body")
	// skill-b: profiles ["review"] - should be skipped
	mustMkdir(t, filepath.Join(src, "skill-b"))
	mustWriteFile(t, filepath.Join(src, "skill-b", "SKILL.md"),
		"---\nname: skill-b\nprofiles: [\"review\"]\n---\n# body")
	// skill-c: profiles ["*"] - should be installed (wildcard)
	mustMkdir(t, filepath.Join(src, "skill-c"))
	mustWriteFile(t, filepath.Join(src, "skill-c", "SKILL.md"),
		"---\nname: skill-c\nprofiles: [\"*\"]\n---\n# body")
	// skill-d: no profiles field - treated as wildcard, always installed
	mustMkdir(t, filepath.Join(src, "skill-d"))
	mustWriteFile(t, filepath.Join(src, "skill-d", "SKILL.md"),
		"---\nname: skill-d\n---\n# body")

	ws := t.TempDir()
	_, err := installSkills(Params{
		Workspace:    ws,
		SkillsSrc:    []string{src},
		SkillProfile: "implement",
	})
	require.NoError(t, err)

	dstBase := filepath.Join(ws, ".claude", "skills")
	require.FileExists(t, filepath.Join(dstBase, "skill-a", "SKILL.md"), "skill-a (implement) must be installed")
	require.NoFileExists(t, filepath.Join(dstBase, "skill-b", "SKILL.md"), "skill-b (review) must be skipped")
	require.FileExists(t, filepath.Join(dstBase, "skill-c", "SKILL.md"), "skill-c (*) must be installed")
	require.FileExists(t, filepath.Join(dstBase, "skill-d", "SKILL.md"), "skill-d (no profiles) must be installed")
}

func TestInstallSkills_EmptyProfile_InstallsAll(t *testing.T) {
	src := t.TempDir()
	for _, name := range []string{"x", "y", "z"} {
		mustMkdir(t, filepath.Join(src, name))
		mustWriteFile(t, filepath.Join(src, name, "SKILL.md"),
			fmt.Sprintf("---\nname: %s\nprofiles: [\"implement\"]\n---\n# body", name))
	}
	ws := t.TempDir()
	_, err := installSkills(Params{
		Workspace:    ws,
		SkillsSrc:    []string{src},
		SkillProfile: "", // empty = fail-open, install all
	})
	require.NoError(t, err)
	dstBase := filepath.Join(ws, ".claude", "skills")
	for _, name := range []string{"x", "y", "z"} {
		require.FileExists(t, filepath.Join(dstBase, name, "SKILL.md"))
	}
}

func TestInstallSkills_CategoryLayout_FiltersCorrectly(t *testing.T) {
	// Simulate the tatara-agent-skills repo layout: skills/<category>/<skill>/SKILL.md
	src := t.TempDir()
	// shared/tatara-platform -> profiles: ["*"]
	mustMkdir(t, filepath.Join(src, "shared", "tatara-platform"))
	mustWriteFile(t, filepath.Join(src, "shared", "tatara-platform", "SKILL.md"),
		"---\nname: tatara-platform\nprofiles: [\"*\"]\n---\n# body")
	// implement/tatara-workflow -> profiles: ["implement"]
	mustMkdir(t, filepath.Join(src, "implement", "tatara-workflow"))
	mustWriteFile(t, filepath.Join(src, "implement", "tatara-workflow", "SKILL.md"),
		"---\nname: tatara-workflow\nprofiles: [\"implement\"]\n---\n# body")
	// brainstorming/tatara-guardrails -> profiles: ["brainstorm"]
	mustMkdir(t, filepath.Join(src, "brainstorming", "tatara-guardrails"))
	mustWriteFile(t, filepath.Join(src, "brainstorming", "tatara-guardrails", "SKILL.md"),
		"---\nname: tatara-guardrails\nprofiles: [\"brainstorm\"]\n---\n# body")

	ws := t.TempDir()
	_, err := installSkills(Params{
		Workspace:    ws,
		SkillsSrc:    []string{src},
		SkillProfile: "implement",
	})
	require.NoError(t, err)

	dstBase := filepath.Join(ws, ".claude", "skills")
	// Skills must be FLATTENED to depth-1 (skills/<skill>/SKILL.md): Claude Code
	// only discovers skills one level under .claude/skills, so the category dir
	// must be dropped.
	require.FileExists(t, filepath.Join(dstBase, "tatara-platform", "SKILL.md"))
	require.FileExists(t, filepath.Join(dstBase, "tatara-workflow", "SKILL.md"))
	require.NoFileExists(t, filepath.Join(dstBase, "tatara-guardrails", "SKILL.md"))
	// No category dirs may leak into the destination.
	require.NoDirExists(t, filepath.Join(dstBase, "shared"))
	require.NoDirExists(t, filepath.Join(dstBase, "implement"))
	require.NoDirExists(t, filepath.Join(dstBase, "brainstorming"))
}

// ---- exec bit preservation ----

func TestInstallSkills_PreservesExecutableBit(t *testing.T) {
	src := t.TempDir()
	skillDir := filepath.Join(src, "my-skill")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	// SKILL.md needed for the dir to be recognized as a skill dir.
	mustWriteFile(t, filepath.Join(skillDir, "SKILL.md"), "---\nname: my-skill\n---\n# body")
	script := filepath.Join(skillDir, "start-server.sh")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755))

	ws := t.TempDir()
	mustInstallSkills(t, Params{Workspace: ws, SkillsSrc: []string{src}})

	dst := filepath.Join(ws, ".claude", "skills", "my-skill", "start-server.sh")
	info, err := os.Stat(dst)
	require.NoError(t, err)
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("exec bit lost: got mode %o, expected 0o111 bits set", info.Mode().Perm())
	}
}

// ---- shadow logging ----

func TestInstallSkills_LogsShadowedSkill(t *testing.T) {
	src1 := t.TempDir()
	src2 := t.TempDir()
	for _, src := range []string{src1, src2} {
		d := filepath.Join(src, "my-skill")
		require.NoError(t, os.MkdirAll(d, 0o755))
		mustWriteFile(t, filepath.Join(d, "SKILL.md"), "# content from "+src)
	}
	ws := t.TempDir()
	var logBuf strings.Builder
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mustInstallSkills(t, Params{Workspace: ws, SkillsSrc: []string{src1, src2}, Log: log})
	require.Contains(t, logBuf.String(), "my-skill")
}

// ---- deploy harness (flat skill dir) ----

func TestInstallSkills_CopiesDeployHarness(t *testing.T) {
	src := t.TempDir()
	skillDir := filepath.Join(src, "tatara-deploy-harness")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	mustWriteFile(t, filepath.Join(skillDir, "SKILL.md"), "---\nname: tatara-deploy-harness\n---\n")

	ws := t.TempDir()
	mustInstallSkills(t, Params{Workspace: ws, SkillsSrc: []string{src}})

	got := filepath.Join(ws, ".claude", "skills", "tatara-deploy-harness", "SKILL.md")
	require.FileExists(t, got)
}

// ---- clone fail-open ----

func TestCloneSkillsRepo_FailOpen(t *testing.T) {
	orig := SkillsCloneRetryDelay
	SkillsCloneRetryDelay = func(int) {}
	defer func() { SkillsCloneRetryDelay = orig }()

	cloneDir := filepath.Join(t.TempDir(), "skills-clone")
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	p := Params{
		SkillsRepo:     "https://github.com/szymonrychu/tatara-agent-skills",
		SkillsRef:      "main",
		SkillsCloneDir: cloneDir,
		M:              m,
	}
	// Fail on "fetch" (the step that breaks for SHA refs), not "clone".
	stubGit := func(_ string, args ...string) error {
		if len(args) > 0 && args[0] == "fetch" {
			return fmt.Errorf("simulated fetch failure")
		}
		return nil
	}
	err := cloneSkillsRepo(p, stubGit)
	require.NoError(t, err, "cloneSkillsRepo must be fail-open")

	// Verify failure counter incremented once, under the skills-repo source.
	require.Equal(t, float64(1),
		sumCounter(t, reg, "ccw_skills_clone_failures_total", map[string]string{"source": "skills_repo"}),
		"clone failure counter must be 1 for source=skills_repo")
	require.Equal(t, float64(0),
		sumCounter(t, reg, "ccw_skills_clone_failures_total", map[string]string{"source": "extra"}),
		"a skills-repo clone failure must not land on the extra-source series")
}

// TestCloneSkillsRepo_UsesFetchNotCloneB asserts that the git command sequence
// uses init+remote+fetch+checkout (never "clone -b"), locking in SHA-ref support.
func TestCloneSkillsRepo_UsesFetchNotCloneB(t *testing.T) {
	orig := SkillsCloneRetryDelay
	SkillsCloneRetryDelay = func(int) {}
	defer func() { SkillsCloneRetryDelay = orig }()

	cloneDir := filepath.Join(t.TempDir(), "skills-clone")
	var cmds []string
	stubGit := func(_ string, args ...string) error {
		cmds = append(cmds, strings.Join(args, " "))
		return nil
	}
	p := Params{
		SkillsRepo:     "https://github.com/example/repo",
		SkillsRef:      "abc1234abc1234abc1234abc1234abc1234abc1234", // fake SHA
		SkillsCloneDir: cloneDir,
	}
	require.NoError(t, cloneSkillsRepo(p, stubGit))

	hasFetch := false
	hasDetach := false
	for _, cmd := range cmds {
		require.NotContains(t, cmd, " clone ", "git clone must not be used for SHA refs")
		require.NotContains(t, cmd, " -b ", "-b branch flag must not be used")
		if strings.HasPrefix(cmd, "fetch") {
			hasFetch = true
		}
		if strings.Contains(cmd, "--detach") {
			hasDetach = true
		}
	}
	require.True(t, hasFetch, "must use git fetch")
	require.True(t, hasDetach, "must use git checkout --detach")
}

// TestCloneSkillsRepo_SHARef_RetryOnFetchFailure asserts that fetch failures
// trigger retries and the clone eventually succeeds on the 3rd attempt.
func TestCloneSkillsRepo_SHARef_RetryOnFetchFailure(t *testing.T) {
	orig := SkillsCloneRetryDelay
	SkillsCloneRetryDelay = func(int) {}
	defer func() { SkillsCloneRetryDelay = orig }()

	cloneDir := filepath.Join(t.TempDir(), "skills-clone")
	fetchCalls := 0
	stubGit := func(_ string, args ...string) error {
		if len(args) > 0 && args[0] == "fetch" {
			fetchCalls++
			if fetchCalls < 3 {
				return fmt.Errorf("simulated fetch failure attempt %d", fetchCalls)
			}
		}
		return nil
	}
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	p := Params{
		SkillsRepo:     "https://github.com/example/repo",
		SkillsRef:      "abc1234",
		SkillsCloneDir: cloneDir,
		M:              m,
	}
	require.NoError(t, cloneSkillsRepo(p, stubGit), "must succeed after retries")
	require.Equal(t, 3, fetchCalls, "must have attempted fetch exactly 3 times")

	// No failure counter since it ultimately succeeded.
	require.Equal(t, float64(0),
		sumCounter(t, reg, "ccw_skills_clone_failures_total", nil),
		"no failure counter when retries succeed")
}

func TestCloneSkillsRepo_NoRepo_NoOp(t *testing.T) {
	var called bool
	stubGit := func(_ string, _ ...string) error { called = true; return nil }
	err := cloneSkillsRepo(Params{SkillsRepo: "", SkillsCloneDir: t.TempDir()}, stubGit)
	require.NoError(t, err)
	require.False(t, called, "no git calls when SkillsRepo is empty")
}

// ---- clone completion sentinel (issue #173, pre-mortem 4) ----
//
// A bare .git is not proof of a finished clone: a kill between "git init" and
// "checkout --detach" leaves one behind, and with a persistent clone dir every
// later boot in that container would skip the clone and install zero skills -
// with cloneSkillsRepo returning nil and the failure counter never moving.
// Reuse is now keyed on a sentinel written only after checkout succeeds.

func TestCloneSkillsRepo_BareGitWithoutSentinel_ReClones(t *testing.T) {
	cloneDir := filepath.Join(t.TempDir(), "skills-clone")
	mustMkdir(t, filepath.Join(cloneDir, ".git"))

	var cmds []string
	stubGit := func(_ string, args ...string) error {
		cmds = append(cmds, strings.Join(args, " "))
		return nil
	}
	p := Params{SkillsRepo: "https://github.com/example/repo", SkillsRef: "main", SkillsCloneDir: cloneDir}
	require.NoError(t, cloneSkillsRepo(p, stubGit))

	require.NotEmpty(t, cmds, "a half-initialised clone dir must be re-cloned, not skipped")
	require.NoDirExists(t, filepath.Join(cloneDir, ".git"),
		"the stale .git must be removed before git init runs")
}

func TestCloneSkillsRepo_SentinelMatchingRef_Skips(t *testing.T) {
	cloneDir := filepath.Join(t.TempDir(), "skills-clone")
	mustMkdir(t, cloneDir)
	mustWriteFile(t, filepath.Join(cloneDir, skillsCloneSentinel), "main\n")

	var cmds []string
	stubGit := func(_ string, args ...string) error {
		cmds = append(cmds, strings.Join(args, " "))
		return nil
	}
	p := Params{SkillsRepo: "https://github.com/example/repo", SkillsRef: "main", SkillsCloneDir: cloneDir}
	require.NoError(t, cloneSkillsRepo(p, stubGit))
	require.Empty(t, cmds, "a completed clone at the requested ref must be reused")
}

func TestCloneSkillsRepo_SentinelDifferentRef_ReClones(t *testing.T) {
	cloneDir := filepath.Join(t.TempDir(), "skills-clone")
	mustMkdir(t, cloneDir)
	mustWriteFile(t, filepath.Join(cloneDir, skillsCloneSentinel), "v1.2.3")

	var cmds []string
	stubGit := func(_ string, args ...string) error {
		cmds = append(cmds, strings.Join(args, " "))
		return nil
	}
	p := Params{SkillsRepo: "https://github.com/example/repo", SkillsRef: "main", SkillsCloneDir: cloneDir}
	require.NoError(t, cloneSkillsRepo(p, stubGit))
	require.NotEmpty(t, cmds, "a ref change between restarts must re-clone")
	require.Equal(t, "main", readSentinel(t, cloneDir), "sentinel must be rewritten to the new ref")
}

// ---- the retry loop's os.RemoveAll is bounded (issue #173, review round 1) ----
//
// SkillsCloneDir is filepath.Dir of the first SKILLS_SRC_DIRS entry, which a
// Project can override via ExtraEnvs. SKILLS_SRC_DIRS=/etc/wrapper/skills - a
// plausible value, and the shape the chart once used as a SOURCE dir - resolves
// the clone dir to /etc/wrapper, the wrapper's own mounted config.

func TestCloneSkillsRepo_NonCloneDir_RefusesToClear(t *testing.T) {
	dir := t.TempDir() // stands in for a mounted /etc/wrapper
	mustWriteFile(t, filepath.Join(dir, "global-claude.md"), "not ours\n")
	mustMkdir(t, filepath.Join(dir, "mcp.d"))

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	var cmds []string
	stubGit := func(_ string, args ...string) error {
		cmds = append(cmds, strings.Join(args, " "))
		return nil
	}

	require.NoError(t, cloneSkillsRepo(Params{
		SkillsRepo: "https://github.com/example/repo", SkillsRef: "main",
		SkillsCloneDir: dir, M: m,
	}, stubGit))

	require.FileExists(t, filepath.Join(dir, "global-claude.md"),
		"a clone target that is not empty and carries no clone marker must never be cleared")
	require.DirExists(t, filepath.Join(dir, "mcp.d"))
	require.Empty(t, cmds, "no git command may run against a directory we refused to clear")
	require.Equal(t, float64(1),
		sumCounter(t, reg, "ccw_skills_clone_failures_total",
			map[string]string{"source": skillsCloneSourceRepo}),
		"refusing to clear the target is a clone failure and must be counted")
}

// ---- .git alone is the marker, not .git anywhere (issue #173, review round 2) ----
//
// A checked-out work repo has a .git too. SKILLS_SRC_DIRS=/workspace/<owner>/<repo>/skills
// derives the clone dir to /workspace/<owner>/<repo>, which Render has already
// cloned, resumed and rescued by the time cloneSkillsRepo runs. Our own
// half-initialised clone is .git-ONLY: init/remote/fetch write nothing into the
// worktree and the sentinel lands immediately after checkout.

func TestCloneSkillsRepo_CheckedOutWorkRepo_RefusesToClear(t *testing.T) {
	dir := t.TempDir() // stands in for /workspace/<owner>/<repo>
	mustMkdir(t, filepath.Join(dir, ".git"))
	mustWriteFile(t, filepath.Join(dir, "README.md"), "the task branch\n")
	mustMkdir(t, filepath.Join(dir, "internal"))

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	var cmds []string
	stubGit := func(_ string, args ...string) error {
		cmds = append(cmds, strings.Join(args, " "))
		return nil
	}

	require.NoError(t, cloneSkillsRepo(Params{
		SkillsRepo: "https://github.com/example/repo", SkillsRef: "main",
		SkillsCloneDir: dir, M: m,
	}, stubGit))

	require.FileExists(t, filepath.Join(dir, "README.md"),
		"a populated git worktree is somebody else's repo and must never be cleared")
	require.DirExists(t, filepath.Join(dir, "internal"))
	require.DirExists(t, filepath.Join(dir, ".git"))
	require.Empty(t, cmds, "no git command may run against a directory we refused to clear")
	require.Equal(t, float64(1),
		sumCounter(t, reg, "ccw_skills_clone_failures_total",
			map[string]string{"source": skillsCloneSourceRepo}),
		"refusing to clear the target is a clone failure and must be counted")
}

func TestCloneSkillsRepo_EmptyDir_Clones(t *testing.T) {
	dir := t.TempDir() // exists, empty: nothing to destroy
	var cmds []string
	stubGit := func(_ string, args ...string) error {
		cmds = append(cmds, strings.Join(args, " "))
		return nil
	}
	require.NoError(t, cloneSkillsRepo(Params{
		SkillsRepo: "https://github.com/example/repo", SkillsRef: "main", SkillsCloneDir: dir,
	}, stubGit))
	require.NotEmpty(t, cmds, "an empty clone target is clonable")
	require.Equal(t, "main", readSentinel(t, dir))
}

func TestCloneSkillsRepo_SentinelOnlyDir_Clones(t *testing.T) {
	// A completed clone whose tree was emptied but whose sentinel survives:
	// the marker vouches for the directory being ours, so a ref change clears it.
	dir := filepath.Join(t.TempDir(), "skills-clone")
	mustMkdir(t, dir)
	mustWriteFile(t, filepath.Join(dir, skillsCloneSentinel), "v1.2.3\n")

	var cmds []string
	stubGit := func(_ string, args ...string) error {
		cmds = append(cmds, strings.Join(args, " "))
		return nil
	}
	require.NoError(t, cloneSkillsRepo(Params{
		SkillsRepo: "https://github.com/example/repo", SkillsRef: "main", SkillsCloneDir: dir,
	}, stubGit))
	require.NotEmpty(t, cmds)
	require.Equal(t, "main", readSentinel(t, dir))
}

func TestCloneSkillsRepo_PopulatedOwnCloneStaleRef_ReClones(t *testing.T) {
	// The realistic reuse path: a completed clone - sentinel, .git and a
	// worktree - whose ref moved. The sentinel is what distinguishes it from
	// the work repo above, so a populated directory carrying it stays clonable.
	dir := filepath.Join(t.TempDir(), "skills-clone")
	mustMkdir(t, filepath.Join(dir, ".git"))
	mustMkdir(t, filepath.Join(dir, "skills"))
	mustWriteFile(t, filepath.Join(dir, skillsCloneSentinel), "v1.2.3\n")

	var cmds []string
	stubGit := func(_ string, args ...string) error {
		cmds = append(cmds, strings.Join(args, " "))
		return nil
	}
	require.NoError(t, cloneSkillsRepo(Params{
		SkillsRepo: "https://github.com/example/repo", SkillsRef: "main", SkillsCloneDir: dir,
	}, stubGit))
	require.NotEmpty(t, cmds, "our own clone at a stale ref must be re-cloned")
	require.NoDirExists(t, filepath.Join(dir, "skills"), "the stale tree must be cleared first")
	require.Equal(t, "main", readSentinel(t, dir))
}

func TestCloneSkillsRepo_WritesSentinelOnlyAfterCheckout(t *testing.T) {
	orig := SkillsCloneRetryDelay
	SkillsCloneRetryDelay = func(int) {}
	defer func() { SkillsCloneRetryDelay = orig }()

	// Checkout always fails: the clone never completes, so no sentinel.
	failDir := filepath.Join(t.TempDir(), "skills-clone")
	failGit := func(_ string, args ...string) error {
		if len(args) > 0 && args[0] == "checkout" {
			return fmt.Errorf("simulated checkout failure")
		}
		return nil
	}
	require.NoError(t, cloneSkillsRepo(Params{
		SkillsRepo: "https://github.com/example/repo", SkillsRef: "main", SkillsCloneDir: failDir,
	}, failGit))
	require.NoFileExists(t, filepath.Join(failDir, skillsCloneSentinel),
		"a clone that never reached checkout must not be marked complete")

	okDir := filepath.Join(t.TempDir(), "skills-clone")
	okGit := func(_ string, _ ...string) error { return nil }
	require.NoError(t, cloneSkillsRepo(Params{
		SkillsRepo: "https://github.com/example/repo", SkillsRef: "abc1234", SkillsCloneDir: okDir,
	}, okGit))
	require.Equal(t, "abc1234", readSentinel(t, okDir))
}

// ---- zero-skill boot is fatal (issue #173, B1) ----

// renderParams is a minimal Render input; each test sets the skills fields.
func renderParams(t *testing.T) Params {
	t.Helper()
	return Params{
		HomeDir:        t.TempDir(),
		Workspace:      t.TempDir(),
		BaseMCP:        []byte(`{"mcpServers":{}}`),
		HookCommand:    "/usr/local/bin/cc-stop-hook",
		PermissionMode: "bypassPermissions",
	}
}

func noopGit(_ string, _ ...string) error { return nil }

// TestRender_ZeroSkillsWithSkillsSrc_Fails is the fail-open fix: a boot that
// installed no skills is a different agent, not a degraded one (no council
// harness, no guardrails, no TDD skill), and must not be stamped ok.
func TestRender_ZeroSkillsWithSkillsSrc_Fails(t *testing.T) {
	p := renderParams(t)
	src := t.TempDir() // exists, contains no skill dirs
	p.SkillsSrc = []string{src}

	err := Render(p, noopGit)
	require.Error(t, err, "Render must fail when SkillsSrc is configured but nothing installed")
	require.Contains(t, err.Error(), "skills")
	require.NoFileExists(t, filepath.Join(p.Workspace, workspaceIdentityFile),
		"workspace identity must not be stamped for a zero-skill boot")
}

// The predicate is the installed count, never cloneSkillsRepo's return: a
// profile that matches no skill installs zero and is equally fatal.
func TestRender_ProfileMatchesNoSkill_Fails(t *testing.T) {
	p := renderParams(t)
	src := t.TempDir()
	mustMkdir(t, filepath.Join(src, "brainstorm-only"))
	mustWriteFile(t, filepath.Join(src, "brainstorm-only", "SKILL.md"),
		"---\nname: brainstorm-only\nprofiles: [\"brainstorm\"]\n---\n")
	p.SkillsSrc = []string{src}
	p.SkillProfile = "implement"

	require.Error(t, Render(p, noopGit))
}

func TestRender_SkillsInstalled_Succeeds(t *testing.T) {
	p := renderParams(t)
	src := t.TempDir()
	mustMkdir(t, filepath.Join(src, "my-skill"))
	mustWriteFile(t, filepath.Join(src, "my-skill", "SKILL.md"),
		"---\nname: my-skill\nprofiles: [\"implement\"]\n---\n")
	p.SkillsSrc = []string{src}
	p.SkillProfile = "implement"

	require.NoError(t, Render(p, noopGit))
	require.FileExists(t, filepath.Join(p.Workspace, ".claude", "skills", "my-skill", "SKILL.md"))
}

// Escape hatch: a deployment that deliberately configures no skill source is
// unaffected by the zero-skill check.
func TestRender_NoSkillsSrc_SkipsZeroSkillCheck(t *testing.T) {
	p := renderParams(t)
	p.SkillsSrc = []string{"", ""}
	require.NoError(t, Render(p, noopGit))
}

// An extra project source cannot substitute for the harness skills: the
// predicate counts installSkills alone.
func TestRender_OnlyExtraSkillSources_StillFails(t *testing.T) {
	p := renderParams(t)
	src := t.TempDir()
	p.SkillsSrc = []string{src}
	p.ExtraSkillSources = []byte(`[{"name":"proj","url":"https://example.com/proj","ref":"main"}]`)

	extraGit := func(dir string, args ...string) error {
		// Materialise a skill in the extra source's checkout.
		if len(args) > 0 && args[0] == "checkout" {
			mustMkdir(t, filepath.Join(dir, "proj-skill"))
			mustWriteFile(t, filepath.Join(dir, "proj-skill", "SKILL.md"), "---\nname: proj-skill\n---\n")
		}
		return nil
	}
	require.Error(t, Render(p, extraGit),
		"a project extra source must not satisfy the harness-skills floor")
}

func readSentinel(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, skillsCloneSentinel))
	require.NoError(t, err)
	return strings.TrimSpace(string(b))
}

// A clone failure with NO SkillsSrc configured hits the escape hatch, not the
// fail-open: the zero-skill check is skipped because the deployment asked for
// no skills at all. This is NOT the production shape - see the test below.
func TestRender_SkillsCloneFailure_NoSkillsSrc_BootContinues(t *testing.T) {
	require.NoError(t, renderWithFailingClone(t, nil),
		"with no SkillsSrc configured the zero-skill check is skipped")
}

// Production shape: SkillsSrc points into the clone that never materialised, so
// the clone failure installs zero skills and the boot is fatal (B1).
func TestRender_SkillsCloneFailure_WithSkillsSrc_Fails(t *testing.T) {
	err := renderWithFailingClone(t, func(p *Params) {
		p.SkillsSrc = []string{filepath.Join(p.SkillsCloneDir, "skills")}
	})
	require.Error(t, err, "a clone failure under production config must fail the boot")
	require.Contains(t, err.Error(), "0 skills installed")
}

// renderWithFailingClone runs Render against a git stub whose "fetch" always
// fails - the step that breaks for SHA refs, and the one a network outage hits.
func renderWithFailingClone(t *testing.T, tweak func(*Params)) error {
	t.Helper()
	orig := SkillsCloneRetryDelay
	SkillsCloneRetryDelay = func(int) {}
	defer func() { SkillsCloneRetryDelay = orig }()

	p := Params{
		HomeDir:        t.TempDir(),
		Workspace:      t.TempDir(),
		BaseMCP:        []byte(`{"mcpServers":{}}`),
		HookCommand:    "/usr/local/bin/cc-stop-hook",
		PermissionMode: "bypassPermissions",
		SkillsRepo:     "https://github.com/szymonrychu/tatara-agent-skills",
		SkillsCloneDir: filepath.Join(t.TempDir(), "skills-clone"),
	}
	if tweak != nil {
		tweak(&p)
	}
	stubGit := func(_ string, args ...string) error {
		if len(args) > 0 && args[0] == "fetch" {
			return fmt.Errorf("network down")
		}
		return nil
	}
	return Render(p, stubGit)
}

// ---- metrics counter via dto ----
// ensure prometheus dto import is used; also exercises the metric via installSkills.
func TestInstallSkills_MetricCounted(t *testing.T) {
	src := t.TempDir()
	mustMkdir(t, filepath.Join(src, "my-skill"))
	mustWriteFile(t, filepath.Join(src, "my-skill", "SKILL.md"), "---\nname: my-skill\nprofiles: [\"implement\"]\n---\n")

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	n, err := installSkills(Params{
		Workspace:    t.TempDir(),
		SkillsSrc:    []string{src},
		SkillProfile: "implement",
		M:            m,
	})
	require.NoError(t, err)
	require.Equal(t, 1, n, "installSkills must return the installed count")

	require.Equal(t, float64(1),
		sumCounter(t, reg, "ccw_skills_installed_total", map[string]string{"profile": "implement"}),
		"installed counter for profile=implement must be 1")
}

// ---- helpers ----

func mustInstallSkills(t *testing.T, p Params) int {
	t.Helper()
	n, err := installSkills(p)
	require.NoError(t, err)
	return n
}

// sumCounter sums every counter in family name whose labels match want. A
// family with no children at all sums to 0 rather than failing, so it reads
// the same whether the CounterVec was never touched or was touched with other
// label values.
func sumCounter(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) float64 {
	t.Helper()
	mf, err := reg.Gather()
	require.NoError(t, err)
	var total float64
	for _, fam := range mf {
		if fam.GetName() != name {
			continue
		}
		for _, mm := range fam.GetMetric() {
			got := make(map[string]string, len(mm.GetLabel()))
			for _, l := range mm.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			match := true
			for k, v := range want {
				if got[k] != v {
					match = false
					break
				}
			}
			if match {
				total += mm.GetCounter().GetValue()
			}
		}
	}
	return total
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(path, 0o755))
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}
