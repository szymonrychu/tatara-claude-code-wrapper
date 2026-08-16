package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The chart's SKILLS_SRC_DIRS has now been wrong twice, in opposite directions,
// and neither failed CI: first "/templates/skills:/etc/wrapper/skills", which
// derived a clone target of /templates (a path the image never creates), then
// "/etc/wrapper/skills/skills", which derives /etc/wrapper/skills - INSIDE the
// chart's own read-only `files` ConfigMap mount, so "git init" fails EROFS,
// installSkills returns 0 and the zero-skill check crash-loops the pod.
//
// Both are the same class: the value silently names a boot-clone target the
// deployment never makes writable. This guard ties the chart value to the code
// default and to a writable volume, so the class fails the build instead.

func chartFile(t *testing.T, name string) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	b, err := os.ReadFile(filepath.Join(root, "charts", "tatara-claude-code-wrapper", "templates", name)) //nolint:gosec // fixed path under the repo's own tree
	require.NoError(t, err)
	return string(b)
}

func chartSkillsSrcDirs(t *testing.T) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^SKILLS_SRC_DIRS:\s*"([^"]*)"`).FindStringSubmatch(chartFile(t, "_helpers.tpl"))
	require.Len(t, m, 2, "_helpers.tpl must set a literal SKILLS_SRC_DIRS")
	return m[1]
}

// The chart value and cmd/wrapper/config.go's default must not drift: the
// wrapper falls back to the code default whenever the operator's pod builder
// does not set SKILLS_SRC_DIRS, so a chart-only value is untested in production.
func TestChartSkillsSrcDirsMatchesCodeDefault(t *testing.T) {
	require.Equal(t, defaultSkillsSrcDirs, chartSkillsSrcDirs(t))
}

func TestChartSkillsCloneDirIsWritable(t *testing.T) {
	cloneDir := skillsCloneDir(chartSkillsSrcDirs(t))
	dep := chartFile(t, "deployment.yaml")

	volName, ok := mountedVolumeAt(dep, cloneDir)
	require.True(t, ok,
		"the boot clone writes to %s, so deployment.yaml must mount a volume there; "+
			"without one the clone lands in the read-only ConfigMap mount and the pod crash-loops",
		cloneDir)
	require.NotContains(t, volumeBlock(t, dep, volName), "configMap:",
		"volume %q backs the boot clone target %s and must be writable, not a ConfigMap projection",
		volName, cloneDir)
}

// mountedVolumeAt returns the volume name mounted at exactly mountPath.
func mountedVolumeAt(deployment, mountPath string) (string, bool) {
	re := regexp.MustCompile(`(?m)^\s*-\s*name:\s*(\S+)\s*\n\s*mountPath:\s*(\S+)\s*$`)
	for _, m := range re.FindAllStringSubmatch(deployment, -1) {
		if strings.Trim(m[2], `"`) == mountPath {
			return m[1], true
		}
	}
	return "", false
}

// volumeBlock returns the lines of the `volumes:` entry named name, up to the
// next entry at the same indent.
func volumeBlock(t *testing.T, deployment, name string) string {
	t.Helper()
	_, vols, found := strings.Cut(deployment, "\n      volumes:\n")
	require.True(t, found, "deployment.yaml must declare a volumes: section")

	var out []string
	in := false
	for _, line := range strings.Split(vols, "\n") {
		if strings.HasPrefix(line, "        - name:") {
			if in {
				break
			}
			in = strings.TrimSpace(line) == "- name: "+name
		}
		if in {
			out = append(out, line)
		}
	}
	require.NotEmpty(t, out, "no volume named %q in deployment.yaml", name)
	return strings.Join(out, "\n")
}
