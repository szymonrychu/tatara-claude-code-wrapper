package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReleasePinsCoverAllProjects is the regression guard for
// tatara-helmfile#290: the release workflow's cd-release "bump" step
// carries a `pins:` array of per-project values files to rewrite on every
// tagged release. That array is hand-maintained prose, not generated from
// any project list, so a project silently dropped (or a new project never
// added) never fails CI - the pin just stops advancing and drifts, exactly
// as project-mtg did (stuck 7 CD bumps behind while project-tatara and
// project-infrastructure kept moving).
//
// This test parses the literal `pins: |` block out of
// .github/workflows/release.yml as JSON and asserts every known project's
// values file is present exactly once. It is deliberately NOT derived from
// a scan of values/*/ (that directory lives in the separate tatara-helmfile
// repo, not here) - the point is a static, explicit list this repo owns, so
// adding a fourth project requires touching this test too.
func TestReleasePinsCoverAllProjects(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	path := filepath.Join(root, ".github", "workflows", "release.yml")
	b, err := os.ReadFile(path) //nolint:gosec // fixed path under the repo's own tree
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	pinsJSON, err := extractPinsBlock(string(b))
	if err != nil {
		t.Fatalf("extract pins block from %s: %v", path, err)
	}

	var pins []struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal([]byte(pinsJSON), &pins); err != nil {
		t.Fatalf("unmarshal pins JSON: %v\n--- raw block ---\n%s", err, pinsJSON)
	}

	got := make(map[string]int, len(pins))
	for _, p := range pins {
		got[p.File]++
	}

	// The full set of tatara-project per-project values files this CD
	// pipeline must keep in lockstep. Matches tatara-operator's own
	// (correct) 5-pin release.yml, which already lists all three.
	want := []string{
		"values/project-tatara/common.yaml",
		"values/project-infrastructure/common.yaml",
		"values/project-mtg/common.yaml",
	}
	for _, w := range want {
		switch got[w] {
		case 0:
			t.Errorf("pins array is missing project values file %q - this is the project-mtg-stranding failure mode (tatara-helmfile#290)", w)
		case 1:
			// ok
		default:
			t.Errorf("pins array lists %q %d times, want exactly once", w, got[w])
		}
	}
}

// extractPinsBlock pulls the literal YAML block-scalar body that follows a
// `pins: |` key out of a release.yml workflow, returning it as a raw string
// (still containing the JSON array). It dedents based on the first
// non-blank line's indentation and stops at the first line indented less
// than that (or EOF).
func extractPinsBlock(doc string) (string, error) {
	lines := strings.Split(doc, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "pins: |" {
			start = i + 1
			break
		}
	}
	if start == -1 {
		return "", fmt.Errorf("no `pins: |` key found")
	}

	baseIndent := -1
	var body []string
	for i := start; i < len(lines); i++ {
		l := lines[i]
		if strings.TrimSpace(l) == "" {
			body = append(body, "")
			continue
		}
		indent := len(l) - len(strings.TrimLeft(l, " "))
		if baseIndent == -1 {
			baseIndent = indent
		}
		if indent < baseIndent {
			break
		}
		body = append(body, l[baseIndent:])
	}
	if baseIndent == -1 {
		return "", fmt.Errorf("`pins: |` block was empty")
	}
	return strings.Join(body, "\n"), nil
}
