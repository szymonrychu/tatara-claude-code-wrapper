package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAgents_WritesImplementerAndExplorer(t *testing.T) {
	home := t.TempDir()
	p := Params{WorkerModel: "sonnet", WorkerEffort: "low"}
	if err := writeAgents(p, home); err != nil {
		t.Fatalf("writeAgents: %v", err)
	}

	implementer, err := os.ReadFile(filepath.Join(home, "agents", "implementer.md"))
	if err != nil {
		t.Fatalf("read implementer.md: %v", err)
	}
	got := string(implementer)
	for _, want := range []string{"name: implementer", "model: sonnet", "effort: low", "mechanical implementation"} {
		if !strings.Contains(got, want) {
			t.Fatalf("implementer.md missing %q, got:\n%s", want, got)
		}
	}

	explorer, err := os.ReadFile(filepath.Join(home, "agents", "explorer.md"))
	if err != nil {
		t.Fatalf("read explorer.md: %v", err)
	}
	got = string(explorer)
	for _, want := range []string{"name: explorer", "model: sonnet", "effort: low", "read-only"} {
		if !strings.Contains(got, want) {
			t.Fatalf("explorer.md missing %q, got:\n%s", want, got)
		}
	}
}

func TestWriteAgents_UsesConfiguredModelAndEffort(t *testing.T) {
	home := t.TempDir()
	p := Params{WorkerModel: "haiku", WorkerEffort: "medium"}
	if err := writeAgents(p, home); err != nil {
		t.Fatalf("writeAgents: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(home, "agents", "implementer.md"))
	if err != nil {
		t.Fatalf("read implementer.md: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, "model: haiku") || !strings.Contains(got, "effort: medium") {
		t.Fatalf("implementer.md did not honor configured model/effort, got:\n%s", got)
	}
}
