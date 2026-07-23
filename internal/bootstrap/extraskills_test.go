package bootstrap

import (
	"encoding/json"
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

// ---- parseExtraSkillSources ----

func TestParseExtraSkillSources(t *testing.T) {
	cases := []struct {
		name  string
		input []byte
		want  []extraSkillSource
	}{
		{
			name:  "valid JSON array",
			input: []byte(`[{"name":"example-skills","url":"https://x/example-skills","ref":"main","subdir":".claude/skills"}]`),
			want: []extraSkillSource{
				{Name: "example-skills", URL: "https://x/example-skills", Ref: "main", Subdir: ".claude/skills"},
			},
		},
		{name: "nil input", input: nil, want: nil},
		{name: "empty bytes", input: []byte(""), want: nil},
		{name: "whitespace only", input: []byte("   \n\t "), want: nil},
		{name: "malformed JSON", input: []byte("{not valid json"), want: nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := parseExtraSkillSources(tc.input)
			require.Equal(t, tc.want, got)
		})
	}
}

// ---- installExtraSkillSources ----

// TestInstallExtraSkillSources_InstallsFromSubdir asserts that a source with a
// Subdir is cloned and its skills are installed from <clone>/<subdir> into
// <workspace>/.claude/skills.
func TestInstallExtraSkillSources_InstallsFromSubdir(t *testing.T) {
	ws := t.TempDir()
	sources := []extraSkillSource{
		{Name: "example-skills", URL: "https://x/example-skills", Ref: "main", Subdir: ".claude/skills"},
	}
	raw, err := json.Marshal(sources)
	require.NoError(t, err)

	// Fake GitRunner: on the checkout step (the last step of the clone
	// sequence), materialize a SKILL.md fixture inside the clone dir so
	// installSkillsFromSrc has something real to walk.
	stubGit := func(dir string, args ...string) error {
		if len(args) > 0 && args[0] == "checkout" {
			skillDir := filepath.Join(dir, ".claude", "skills", "build-thing")
			require.NoError(t, os.MkdirAll(skillDir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
				[]byte("---\nname: build-thing\n---\n# body"), 0o644))
		}
		return nil
	}

	p := Params{
		Workspace:         ws,
		ExtraSkillSources: raw,
	}
	require.NoError(t, installExtraSkillSources(p, stubGit))

	require.FileExists(t, filepath.Join(ws, ".claude", "skills", "build-thing", "SKILL.md"))
}

// TestInstallExtraSkillSources_PerSourceFailureIsolation asserts that one
// source failing to clone (fetch error) does not stop the remaining sources
// from being cloned and installed, and that the failure is logged and
// counted.
func TestInstallExtraSkillSources_PerSourceFailureIsolation(t *testing.T) {
	ws := t.TempDir()
	sources := []extraSkillSource{
		{Name: "bad-source", URL: "https://github.com/example/bad", Ref: "main"},
		{Name: "good-source", URL: "https://github.com/example/good", Ref: "main", Subdir: "skills"},
	}
	raw, err := json.Marshal(sources)
	require.NoError(t, err)

	stubGit := func(dir string, args ...string) error {
		if len(args) > 0 && args[0] == "fetch" && strings.Contains(dir, "bad-source") {
			return fmt.Errorf("simulated fetch failure")
		}
		if len(args) > 0 && args[0] == "checkout" && strings.Contains(dir, "good-source") {
			skillDir := filepath.Join(dir, "skills", "good-skill")
			require.NoError(t, os.MkdirAll(skillDir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
				[]byte("---\nname: good-skill\n---\n# body"), 0o644))
		}
		return nil
	}

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	var logBuf strings.Builder
	log := slog.New(slog.NewTextHandler(&logBuf, nil))

	p := Params{
		Workspace:         ws,
		ExtraSkillSources: raw,
		Log:               log,
		M:                 m,
	}
	require.NoError(t, installExtraSkillSources(p, stubGit))

	require.NoDirExists(t, filepath.Join(ws, ".claude", "skills", "bad-source"))
	require.FileExists(t, filepath.Join(ws, ".claude", "skills", "good-skill", "SKILL.md"))
	require.Contains(t, logBuf.String(), "bad-source")

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
	require.Equal(t, float64(1), failCount, "bad-source clone failure must be counted once")
}

// TestInstallExtraSkillSources_RejectsTraversalName asserts that a Name
// containing path-traversal segments is rejected by the ^[a-z0-9-]+$ check
// before any filesystem path is built from it or git is invoked - so
// os.RemoveAll/clone can never touch a path outside the extra-skills base.
func TestInstallExtraSkillSources_RejectsTraversalName(t *testing.T) {
	ws := t.TempDir()
	sources := []extraSkillSource{
		{Name: "../escape", URL: "https://x/escape", Ref: "main"},
	}
	raw, err := json.Marshal(sources)
	require.NoError(t, err)

	gitCalls := 0
	stubGit := func(dir string, args ...string) error {
		gitCalls++
		return nil
	}

	var logBuf strings.Builder
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	p := Params{
		Workspace:         ws,
		ExtraSkillSources: raw,
		Log:               log,
	}
	require.NoError(t, installExtraSkillSources(p, stubGit))

	require.Equal(t, 0, gitCalls, "invalid name must be rejected before any git invocation")
	require.NoDirExists(t, filepath.Join(ws, "escape"))
	require.Contains(t, logBuf.String(), "extra skill source invalid name; skipping")
}

// TestInstallExtraSkillSources_RejectsUnsafeSubdir asserts that a Subdir
// which is absolute, or whose filepath.Clean form starts with "..", is
// rejected before it is joined onto cloneDir (which would let it escape the
// clone).
func TestInstallExtraSkillSources_RejectsUnsafeSubdir(t *testing.T) {
	cases := []struct {
		name   string
		subdir string
	}{
		{"absolute", "/etc"},
		{"traversal", "../../etc"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			sources := []extraSkillSource{
				{Name: "example-skills", URL: "https://x/example-skills", Ref: "main", Subdir: tc.subdir},
			}
			raw, err := json.Marshal(sources)
			require.NoError(t, err)

			gitCalls := 0
			stubGit := func(dir string, args ...string) error {
				gitCalls++
				return nil
			}

			var logBuf strings.Builder
			log := slog.New(slog.NewTextHandler(&logBuf, nil))
			p := Params{
				Workspace:         ws,
				ExtraSkillSources: raw,
				Log:               log,
			}
			require.NoError(t, installExtraSkillSources(p, stubGit))

			require.Equal(t, 0, gitCalls, "unsafe subdir must be rejected before cloning")
			require.Contains(t, logBuf.String(), "extra skill source invalid subdir; skipping")
		})
	}
}

// TestInstallExtraSkillSources_RejectsNonHTTPURL asserts that only http(s)
// URLs are accepted, blocking ext:: transports, file:// URLs, and
// leading-dash strings that git could otherwise interpret as flags.
func TestInstallExtraSkillSources_RejectsNonHTTPURL(t *testing.T) {
	cases := []string{"ext::sh -c 'true'", "file:///etc/passwd", "-oProxyCommand=x"}
	for _, url := range cases {
		t.Run(url, func(t *testing.T) {
			ws := t.TempDir()
			sources := []extraSkillSource{
				{Name: "example-skills", URL: url, Ref: "main"},
			}
			raw, err := json.Marshal(sources)
			require.NoError(t, err)

			gitCalls := 0
			stubGit := func(dir string, args ...string) error {
				gitCalls++
				return nil
			}

			var logBuf strings.Builder
			log := slog.New(slog.NewTextHandler(&logBuf, nil))
			p := Params{
				Workspace:         ws,
				ExtraSkillSources: raw,
				Log:               log,
			}
			require.NoError(t, installExtraSkillSources(p, stubGit))

			require.Equal(t, 0, gitCalls, "non-http(s) URL must be rejected before any git invocation")
			require.Contains(t, logBuf.String(), "extra skill source invalid url; skipping")
		})
	}
}

// TestInstallExtraSkillSources_SkipsDuplicateName asserts that a second
// source entry reusing an earlier Name is skipped (and never cloned) while
// the first occurrence still installs normally.
func TestInstallExtraSkillSources_SkipsDuplicateName(t *testing.T) {
	ws := t.TempDir()
	sources := []extraSkillSource{
		{Name: "example-skills", URL: "https://x/example-skills", Ref: "main", Subdir: "skills"},
		{Name: "example-skills", URL: "https://x/example-skills-2", Ref: "main", Subdir: "skills"},
	}
	raw, err := json.Marshal(sources)
	require.NoError(t, err)

	clones := 0
	stubGit := func(dir string, args ...string) error {
		if len(args) > 0 && args[0] == "checkout" {
			clones++
			skillDir := filepath.Join(dir, "skills", fmt.Sprintf("skill-%d", clones))
			require.NoError(t, os.MkdirAll(skillDir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
				[]byte("---\nname: skill\n---\n# body"), 0o644))
		}
		return nil
	}

	var logBuf strings.Builder
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	p := Params{
		Workspace:         ws,
		ExtraSkillSources: raw,
		Log:               log,
	}
	require.NoError(t, installExtraSkillSources(p, stubGit))

	require.Equal(t, 1, clones, "duplicate-named entry must be skipped before cloning")
	require.Contains(t, logBuf.String(), "duplicate extra skill source name; skipping")
}
