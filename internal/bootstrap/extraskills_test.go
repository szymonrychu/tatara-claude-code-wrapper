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
			input: []byte(`[{"name":"mtg-decks","url":"https://github.com/example/mtg-decks","ref":"main","subdir":".claude/skills"}]`),
			want: []extraSkillSource{
				{Name: "mtg-decks", URL: "https://github.com/example/mtg-decks", Ref: "main", Subdir: ".claude/skills"},
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
		{Name: "mtg-decks", URL: "https://github.com/example/mtg-decks", Ref: "main", Subdir: ".claude/skills"},
	}
	raw, err := json.Marshal(sources)
	require.NoError(t, err)

	// Fake GitRunner: on the checkout step (the last step of the clone
	// sequence), materialize a SKILL.md fixture inside the clone dir so
	// installSkillsFromSrc has something real to walk.
	stubGit := func(dir string, args ...string) error {
		if len(args) > 0 && args[0] == "checkout" {
			skillDir := filepath.Join(dir, ".claude", "skills", "build-deck")
			require.NoError(t, os.MkdirAll(skillDir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
				[]byte("---\nname: build-deck\n---\n# body"), 0o644))
		}
		return nil
	}

	p := Params{
		Workspace:         ws,
		ExtraSkillSources: raw,
	}
	require.NoError(t, installExtraSkillSources(p, stubGit))

	require.FileExists(t, filepath.Join(ws, ".claude", "skills", "build-deck", "SKILL.md"))
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
