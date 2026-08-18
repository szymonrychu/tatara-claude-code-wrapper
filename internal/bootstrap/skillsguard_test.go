package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
)

// counterWithLabel sums the samples of family whose labels match want.
func counterWithLabel(t *testing.T, reg *prometheus.Registry, family string, want map[string]string) float64 {
	t.Helper()
	mf, err := reg.Gather()
	require.NoError(t, err)
	var total float64
	for _, fam := range mf {
		if fam.GetName() != family {
			continue
		}
		for _, mm := range fam.GetMetric() {
			got := map[string]string{}
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

// TestInstallSkills_ReturnsInstalledCount pins the predicate B1 keys on: the
// number of skills actually written, not cloneSkillsRepo's (always nil) return.
func TestInstallSkills_ReturnsInstalledCount(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "alpha"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "alpha", "SKILL.md"), []byte("# alpha"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(src, "beta"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "beta", "SKILL.md"), []byte("# beta"), 0o644))

	n, err := installSkills(Params{Workspace: t.TempDir(), SkillsSrc: []string{src}})
	require.NoError(t, err)
	require.Equal(t, 2, n)
}

// TestInstallSkills_ZeroWhenSourceMissing: a missing source dir is still not an
// error here - installSkills reports the count and Render owns the decision.
func TestInstallSkills_ZeroWhenSourceMissing(t *testing.T) {
	n, err := installSkills(Params{
		Workspace: t.TempDir(),
		SkillsSrc: []string{filepath.Join(t.TempDir(), "never-cloned")},
	})
	require.NoError(t, err)
	require.Zero(t, n)
}

// TestRender_FailsWhenSkillsSrcConfiguredButNothingInstalled is B1. A boot that
// installs zero skills produces a different agent, not a degraded one: no
// council harness, no guardrails, no TDD skill. Nothing is baked into the image
// (the 20 baked skills were dropped when fail-open landed), so the clone is the
// only source and a missing tree must be loud.
func TestRender_FailsWhenSkillsSrcConfiguredButNothingInstalled(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	err := Render(Params{
		HomeDir:   t.TempDir(),
		Workspace: t.TempDir(),
		SkillsSrc: []string{filepath.Join(t.TempDir(), "empty-clone")},
		M:         m,
	}, func(string, ...string) error { return nil })
	require.Error(t, err)
	require.Contains(t, err.Error(), "no skills installed")
	require.Equal(t, float64(1),
		counterWithLabel(t, reg, "ccw_bootstrap_render_total", map[string]string{"result": "fail"}))
}

// TestRender_SucceedsWithNoSkillsSrcConfigured is the escape hatch: a
// deployment that deliberately runs skill-less sets no SkillsSrc at all and is
// unaffected by B1.
func TestRender_SucceedsWithNoSkillsSrcConfigured(t *testing.T) {
	require.NoError(t, Render(Params{
		HomeDir:   t.TempDir(),
		Workspace: t.TempDir(),
	}, func(string, ...string) error { return nil }))
}

// TestRender_SucceedsWhenSkillsInstalled keeps the happy path honest.
func TestRender_SucceedsWhenSkillsInstalled(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "handoff"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "handoff", "SKILL.md"), []byte("# handoff"), 0o644))

	require.NoError(t, Render(Params{
		HomeDir:   t.TempDir(),
		Workspace: t.TempDir(),
		SkillsSrc: []string{src},
	}, func(string, ...string) error { return nil }))
}

// gitStub emulates the parts of git cloneSkillsRepo depends on: "init <dir>"
// creates the target and its .git, "checkout" writes a marker file so a test can
// tell one checkout from another. fetchErr, when non-nil, fails every fetch.
func gitStub(t *testing.T, tree string, fetchErr error) (GitRunner, *bool) {
	t.Helper()
	fetched := false
	return func(dir string, a ...string) error {
		switch {
		case len(a) > 2 && a[0] == "init":
			return os.MkdirAll(filepath.Join(a[2], ".git"), 0o755)
		case len(a) > 0 && a[0] == "fetch":
			fetched = true
			return fetchErr
		case len(a) > 0 && a[0] == "checkout":
			return os.WriteFile(filepath.Join(dir, "tree.txt"), []byte(tree), 0o644)
		}
		return nil
	}, &fetched
}

// TestCloneSkillsRepo_ReclonesWhenSentinelMissing covers pre-mortem 4: a
// container killed between "git init" and "checkout --detach" leaves .git
// present and the tree empty. The old bare .git stat skip made every later boot
// in that container install zero skills, with cloneSkillsRepo returning nil and
// no failure counted.
func TestCloneSkillsRepo_ReclonesWhenSentinelMissing(t *testing.T) {
	cloneDir := filepath.Join(t.TempDir(), "skills-clone")
	require.NoError(t, os.MkdirAll(filepath.Join(cloneDir, ".git"), 0o755))

	git, fetched := gitStub(t, "fresh", nil)
	require.NoError(t, cloneSkillsRepo(Params{
		SkillsRepo: "https://example.invalid/skills", SkillsRef: "v1", SkillsCloneDir: cloneDir,
	}, git))
	require.True(t, *fetched, "half-initialised clone dir must be re-cloned, not reused")
	requireFileContent(t, filepath.Join(cloneDir, skillsRefSentinel), "v1")
	requireFileContent(t, filepath.Join(cloneDir, "tree.txt"), "fresh")
}

// TestCloneSkillsRepo_ReclonesWhenSentinelRefDiffers: the pin moved between
// restarts in the same container, so the existing checkout is the wrong tree.
func TestCloneSkillsRepo_ReclonesWhenSentinelRefDiffers(t *testing.T) {
	cloneDir := filepath.Join(t.TempDir(), "skills-clone")
	require.NoError(t, os.MkdirAll(filepath.Join(cloneDir, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cloneDir, skillsRefSentinel), []byte("v1"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(cloneDir, "tree.txt"), []byte("stale"), 0o644))

	git, fetched := gitStub(t, "fresh", nil)
	require.NoError(t, cloneSkillsRepo(Params{
		SkillsRepo: "https://example.invalid/skills", SkillsRef: "v2", SkillsCloneDir: cloneDir,
	}, git))
	require.True(t, *fetched, "a ref change must re-clone")
	requireFileContent(t, filepath.Join(cloneDir, skillsRefSentinel), "v2")
	requireFileContent(t, filepath.Join(cloneDir, "tree.txt"), "fresh")
}

// TestCloneSkillsRepo_KeepsExistingCloneWhenFetchFails is the offline floor. A
// pod restarting onto a persistent clone dir written by an older wrapper has no
// sentinel, so it re-clones - but clobbering the good tree BEFORE the fetch
// would turn a survivable GitHub outage into a dead boot now that Render fails
// on zero skills. The new tree is staged beside the old one and only promoted
// once checkout succeeds.
func TestCloneSkillsRepo_KeepsExistingCloneWhenFetchFails(t *testing.T) {
	orig := SkillsCloneRetryDelay
	SkillsCloneRetryDelay = func(int) {}
	defer func() { SkillsCloneRetryDelay = orig }()

	base := t.TempDir()
	cloneDir := filepath.Join(base, "skills-clone")
	require.NoError(t, os.MkdirAll(filepath.Join(cloneDir, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cloneDir, "tree.txt"), []byte("previous"), 0o644))

	git, _ := gitStub(t, "fresh", os.ErrPermission)
	require.NoError(t, cloneSkillsRepo(Params{
		SkillsRepo: "https://example.invalid/skills", SkillsRef: "v2", SkillsCloneDir: cloneDir,
	}, git))

	requireFileContent(t, filepath.Join(cloneDir, "tree.txt"), "previous")
	require.NoFileExists(t, filepath.Join(cloneDir, skillsRefSentinel),
		"a surviving old tree must not be stamped with the ref it does not hold")
	entries, err := os.ReadDir(base)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the staging dir must be cleaned up: %v", entries)
}

// TestCloneSkillsRepo_RefusesToReplaceNonCloneTarget: SkillsCloneDir is derived
// by filepath.Dir from operator-supplied SKILLS_SRC_DIRS and is never validated.
// Pointed at a config mount it would delete the wrapper's own inputs, so the
// destructive promote runs only over an absent dir or a previous skills clone.
func TestCloneSkillsRepo_RefusesToReplaceNonCloneTarget(t *testing.T) {
	orig := SkillsCloneRetryDelay
	SkillsCloneRetryDelay = func(int) {}
	defer func() { SkillsCloneRetryDelay = orig }()

	cloneDir := filepath.Join(t.TempDir(), "etc-wrapper")
	require.NoError(t, os.MkdirAll(cloneDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cloneDir, "allowed-tools.txt"), []byte("Bash"), 0o644))

	git, _ := gitStub(t, "fresh", nil)
	require.NoError(t, cloneSkillsRepo(Params{
		SkillsRepo: "https://example.invalid/skills", SkillsRef: "v1", SkillsCloneDir: cloneDir,
	}, git))
	requireFileContent(t, filepath.Join(cloneDir, "allowed-tools.txt"), "Bash")
}

func requireFileContent(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, want, string(b))
}

// TestCloneSkillsRepo_ReusesCloneWhenSentinelMatches keeps the persistent-clone
// fast path: the sentinel is written only after checkout succeeds.
func TestCloneSkillsRepo_ReusesCloneWhenSentinelMatches(t *testing.T) {
	cloneDir := filepath.Join(t.TempDir(), "skills-clone")
	require.NoError(t, os.MkdirAll(filepath.Join(cloneDir, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cloneDir, skillsRefSentinel), []byte("v1"), 0o644))

	var called bool
	git := func(_ string, a ...string) error {
		if len(a) > 0 && a[0] != "config" {
			called = true
		}
		return nil
	}
	require.NoError(t, cloneSkillsRepo(Params{
		SkillsRepo: "https://example.invalid/skills", SkillsRef: "v1", SkillsCloneDir: cloneDir,
	}, git))
	require.False(t, called, "matching sentinel must skip the clone")
}

// TestCloneSkillsRepo_WritesSentinelAfterCheckout: the sentinel is the proof the
// checkout completed, so it must not exist unless every git step succeeded.
func TestCloneSkillsRepo_WritesSentinelAfterCheckout(t *testing.T) {
	cloneDir := filepath.Join(t.TempDir(), "skills-clone")
	git, _ := gitStub(t, "fresh", nil)
	require.NoError(t, cloneSkillsRepo(Params{
		SkillsRepo: "https://example.invalid/skills", SkillsRef: "abc123", SkillsCloneDir: cloneDir,
	}, git))
	requireFileContent(t, filepath.Join(cloneDir, skillsRefSentinel), "abc123")
}

// TestCloneSkillsRepo_NoSentinelOnFailure guards the other half: a failed clone
// must leave nothing a later boot can mistake for a completed one.
func TestCloneSkillsRepo_NoSentinelOnFailure(t *testing.T) {
	orig := SkillsCloneRetryDelay
	SkillsCloneRetryDelay = func(int) {}
	defer func() { SkillsCloneRetryDelay = orig }()

	cloneDir := filepath.Join(t.TempDir(), "skills-clone")
	git, _ := gitStub(t, "fresh", os.ErrPermission)
	require.NoError(t, cloneSkillsRepo(Params{
		SkillsRepo: "https://example.invalid/skills", SkillsRef: "abc123", SkillsCloneDir: cloneDir,
	}, git))
	require.NoFileExists(t, filepath.Join(cloneDir, skillsRefSentinel))
}

// TestCloneSkillsRepo_FailureCounterCarriesSourceLabel resolves pre-mortem 5:
// the skills-repo clone and a bad TATARA_EXTRA_SKILL_SOURCES entry share one
// counter, but only the former is a fleet-wide skills outage. The label is added
// now, while the series is still dark, so the follow-up alert can key on
// source="skills_repo" alone.
func TestCloneSkillsRepo_FailureCounterCarriesSourceLabel(t *testing.T) {
	orig := SkillsCloneRetryDelay
	SkillsCloneRetryDelay = func(int) {}
	defer func() { SkillsCloneRetryDelay = orig }()

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	git := func(_ string, a ...string) error {
		if len(a) > 0 && a[0] == "fetch" {
			return os.ErrPermission
		}
		return nil
	}
	require.NoError(t, cloneSkillsRepo(Params{
		SkillsRepo:     "https://example.invalid/skills",
		SkillsCloneDir: filepath.Join(t.TempDir(), "skills-clone"),
		M:              m,
	}, git))

	require.Equal(t, float64(1), counterWithLabel(t, reg,
		"ccw_skills_clone_failures_total", map[string]string{"source": "skills_repo"}))
	require.Zero(t, counterWithLabel(t, reg,
		"ccw_skills_clone_failures_total", map[string]string{"source": "extra"}))
}
