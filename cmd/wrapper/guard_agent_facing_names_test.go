package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// workflow is the slice of a GitHub Actions workflow these guards assert on.
type workflow struct {
	Jobs map[string]struct {
		Needs any `yaml:"needs"`
		Steps []struct {
			Name string `yaml:"name"`
			Uses string `yaml:"uses"`
			Run  string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func loadWorkflow(t *testing.T, name string) workflow {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, ".github", "workflows", name)) //nolint:gosec // fixed path under the repo's own tree
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var w workflow
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return w
}

// #193: the cli MCP-tools guard runs on the WRONG SIDE OF THE TAG. release.yml
// cuts and pushes the semver tag first (cd-release mode: tag ends in `git push
// origin refs/tags/$next`) and only then runs build.sh, so a bump that drops a
// tool the prompt text names fails the publish AFTER the tag is public - the
// dangling "published tag with no image and no chart behind it" that ci.yml's
// own pin-guard comment describes, and that !187 records happening on
// 2026-07-30.
//
// release.yml's header claimed the opposite ("the cascade stalls before any
// tag/pin is propagated"). This asserts the claim rather than the comment.
func TestReleaseRunsTheBuildGuardBeforeCuttingTheTag(t *testing.T) {
	steps := loadWorkflow(t, "release.yml").Jobs["release"].Steps
	if len(steps) == 0 {
		t.Fatal("release.yml has no `release` job steps")
	}

	indexOf := func(pred func(string, string) bool) int {
		for i, s := range steps {
			if pred(s.Name, s.Run+" "+s.Uses) {
				return i
			}
		}
		return -1
	}

	guard := indexOf(func(_, body string) bool {
		return strings.Contains(body, "build.sh") && strings.Contains(body, " guard")
	})
	tag := indexOf(func(name, _ string) bool { return name == "cut tag" })
	image := indexOf(func(_, body string) bool {
		return strings.Contains(body, "build.sh") && strings.Contains(body, " image")
	})
	buildctl := indexOf(func(name, _ string) bool { return name == "install buildctl" })

	for _, c := range []struct {
		what string
		at   int
	}{
		{"build.sh <repo> guard step", guard},
		{"`cut tag` step", tag},
		{"build.sh <repo> image step", image},
		{"`install buildctl` step", buildctl},
	} {
		if c.at == -1 {
			t.Fatalf("release.yml's `release` job has no %s", c.what)
		}
	}

	if guard >= tag {
		t.Errorf("the cli MCP-tools build-guard runs at step %d, at or after `cut tag` at step %d. "+
			"A guard failure then leaves a published semver tag with no image and no chart behind it, "+
			"which is exactly the state ci.yml's pin-guard comment calls a dangling release someone "+
			"cleans up by hand (#193).", guard, tag)
	}
	if buildctl >= guard {
		t.Errorf("`install buildctl` is at step %d but the build-guard that needs buildctl is at step %d", buildctl, guard)
	}
	if image <= tag {
		t.Errorf("the image publish is at step %d, at or before `cut tag` at step %d; "+
			"the publish must still key off the tag the `cut tag` step computed", image, tag)
	}
}

// release.yml passing ` guard` is only half the contract; build.sh has to MEAN
// it. Against a build.sh that ignored $2 the reordered workflow would build and
// PUSH the runtime image from the pre-tag step - a worse outcome than the one
// the reorder fixes, and one the ordering test above cannot see.
//
// Argument validation happens before build.sh touches env, buildkitd or the
// network, so this exercises the real script rather than asserting on its text.
func TestBuildScriptTakesAModeArgument(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	script := filepath.Join(root, ".github", "ci", "build.sh")

	// Missing env makes every accepted mode exit 1; only an unknown mode exits 2.
	const usageExit = 2
	for _, tc := range []struct {
		mode string
		want bool // want the usage rejection
	}{
		{"guard", false},
		{"image", false},
		{"all", false},
		{"", false}, // omitted entirely: defaults to all, so Makefile/manual use is unchanged
		{"publish", true},
	} {
		args := []string{script, "tatara-claude-code-wrapper"}
		if tc.mode != "" {
			args = append(args, tc.mode)
		}
		cmd := exec.Command("bash", args...) //nolint:gosec // args are this test's own literals plus a path under the repo tree
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		cmd.Dir = root
		out, err := cmd.CombinedOutput()

		var code int
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatalf("run build.sh %q: %v", tc.mode, err)
		}

		if got := code == usageExit; got != tc.want {
			t.Errorf("build.sh with mode %q exited %d, want usage-rejection=%v.\n%s",
				tc.mode, code, tc.want, out)
		}
	}
}

// The offline compensating control for the #136 half of the Dockerfile
// test-guard stage has to be WIRED, not merely present. `test-guard` itself is
// the cautionary tale: a stage hardened against going green by running nothing,
// which nothing on the PR path runs at all.
func TestCIRunsTheAgentFacingNamesGuard(t *testing.T) {
	jobs := loadWorkflow(t, "ci.yml").Jobs
	job, ok := jobs["name-guard"]
	if !ok {
		t.Fatalf("ci.yml has no `name-guard` job; got jobs %v", keysOf(jobs))
	}

	var runsIt bool
	for _, s := range job.Steps {
		if strings.Contains(s.Run, "check-agent-facing-names.sh") {
			runsIt = true
		}
	}
	if !runsIt {
		t.Error("ci.yml's `name-guard` job never runs .github/ci/check-agent-facing-names.sh")
	}

	root, err := filepath.Abs(filepath.Join("..", "..", ".github", "ci", "check-agent-facing-names.sh"))
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("ci.yml runs a guard script that does not exist: %v", err)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
