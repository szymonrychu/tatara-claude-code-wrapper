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
	err := installSkills(Params{
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
	err := installSkills(Params{
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
	err := installSkills(Params{
		Workspace:    ws,
		SkillsSrc:    []string{src},
		SkillProfile: "implement",
	})
	require.NoError(t, err)

	dstBase := filepath.Join(ws, ".claude", "skills")
	require.FileExists(t, filepath.Join(dstBase, "shared", "tatara-platform", "SKILL.md"))
	require.FileExists(t, filepath.Join(dstBase, "implement", "tatara-workflow", "SKILL.md"))
	require.NoFileExists(t, filepath.Join(dstBase, "brainstorming", "tatara-guardrails", "SKILL.md"))
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
	require.NoError(t, installSkills(Params{Workspace: ws, SkillsSrc: []string{src}}))

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
	require.NoError(t, installSkills(Params{Workspace: ws, SkillsSrc: []string{src1, src2}, Log: log}))
	require.Contains(t, logBuf.String(), "my-skill")
}

// ---- deploy harness (flat skill dir) ----

func TestInstallSkills_CopiesDeployHarness(t *testing.T) {
	src := t.TempDir()
	skillDir := filepath.Join(src, "tatara-deploy-harness")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	mustWriteFile(t, filepath.Join(skillDir, "SKILL.md"), "---\nname: tatara-deploy-harness\n---\n")

	ws := t.TempDir()
	require.NoError(t, installSkills(Params{Workspace: ws, SkillsSrc: []string{src}}))

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
	stubGit := func(_ string, args ...string) error {
		if len(args) > 0 && args[0] == "clone" {
			return fmt.Errorf("simulated clone failure")
		}
		return nil
	}
	err := cloneSkillsRepo(p, stubGit)
	require.NoError(t, err, "cloneSkillsRepo must be fail-open")

	// Verify failure counter incremented once.
	mf, err := reg.Gather()
	require.NoError(t, err)
	var failCount float64
	for _, fam := range mf {
		if fam.GetName() == "wrapper_skills_clone_failures_total" {
			for _, mm := range fam.GetMetric() {
				failCount += mm.GetCounter().GetValue()
			}
		}
	}
	require.Equal(t, float64(1), failCount, "clone failure counter must be 1")
}

func TestCloneSkillsRepo_NoRepo_NoOp(t *testing.T) {
	var called bool
	stubGit := func(_ string, _ ...string) error { called = true; return nil }
	err := cloneSkillsRepo(Params{SkillsRepo: "", SkillsCloneDir: t.TempDir()}, stubGit)
	require.NoError(t, err)
	require.False(t, called, "no git calls when SkillsRepo is empty")
}

func TestRender_SkillsCloneFailure_BootContinues(t *testing.T) {
	orig := SkillsCloneRetryDelay
	SkillsCloneRetryDelay = func(int) {}
	defer func() { SkillsCloneRetryDelay = orig }()

	cloneDir := filepath.Join(t.TempDir(), "skills-clone")
	p := Params{
		HomeDir:        t.TempDir(),
		Workspace:      t.TempDir(),
		BaseMCP:        []byte(`{"mcpServers":{}}`),
		HookCommand:    "/usr/local/bin/cc-stop-hook",
		PermissionMode: "bypassPermissions",
		SkillsRepo:     "https://github.com/szymonrychu/tatara-agent-skills",
		SkillsCloneDir: cloneDir,
	}
	stubGit := func(_ string, args ...string) error {
		if len(args) > 0 && args[0] == "clone" {
			return fmt.Errorf("network down")
		}
		return nil
	}
	require.NoError(t, Render(p, stubGit), "Render must succeed even when skills clone fails")
}

// ---- metrics counter via dto ----
// ensure prometheus dto import is used; also exercises the metric via installSkills.
func TestInstallSkills_MetricCounted(t *testing.T) {
	src := t.TempDir()
	mustMkdir(t, filepath.Join(src, "my-skill"))
	mustWriteFile(t, filepath.Join(src, "my-skill", "SKILL.md"), "---\nname: my-skill\nprofiles: [\"implement\"]\n---\n")

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	err := installSkills(Params{
		Workspace:    t.TempDir(),
		SkillsSrc:    []string{src},
		SkillProfile: "implement",
		M:            m,
	})
	require.NoError(t, err)

	mf, err := reg.Gather()
	require.NoError(t, err)
	var total float64
	for _, fam := range mf {
		if fam.GetName() == "wrapper_skills_installed_total" {
			for _, mm := range fam.GetMetric() {
				for _, l := range mm.GetLabel() {
					if l.GetName() == "profile" && l.GetValue() == "implement" {
						total += mm.GetCounter().GetValue()
					}
				}
			}
		}
	}
	require.Equal(t, float64(1), total, "installed counter for profile=implement must be 1")
}

// ---- helpers ----

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(path, 0o755))
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}
