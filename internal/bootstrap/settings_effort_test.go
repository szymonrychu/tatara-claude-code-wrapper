package bootstrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func readSettings(t *testing.T, home string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal settings.json: %v", err)
	}
	return m
}

func TestWriteSettings_EffortLevelWhenSet(t *testing.T) {
	home := t.TempDir()
	if err := writeSettings(Params{HookCommand: "/x", Effort: "xhigh"}, home); err != nil {
		t.Fatalf("writeSettings: %v", err)
	}
	m := readSettings(t, home)
	if m["effortLevel"] != "xhigh" {
		t.Fatalf("effortLevel = %v, want xhigh", m["effortLevel"])
	}
}

func TestWriteSettings_NoEffortLevelWhenEmpty(t *testing.T) {
	home := t.TempDir()
	if err := writeSettings(Params{HookCommand: "/x"}, home); err != nil {
		t.Fatalf("writeSettings: %v", err)
	}
	m := readSettings(t, home)
	if _, ok := m["effortLevel"]; ok {
		t.Fatalf("effortLevel must be absent when Effort empty, got %v", m["effortLevel"])
	}
}
