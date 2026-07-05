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

func TestWriteSettings_BypassDefaultMode_AutoApprovesDispatch(t *testing.T) {
	home := t.TempDir()
	// Mirrors the live boot: bypassPermissions, empty allow-list.
	if err := writeSettings(Params{HookCommand: "/x", PermissionMode: "bypassPermissions"}, home); err != nil {
		t.Fatalf("writeSettings: %v", err)
	}
	m := readSettings(t, home)
	perms, ok := m["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("permissions block missing: %v", m["permissions"])
	}
	if perms["defaultMode"] != "bypassPermissions" {
		t.Fatalf("defaultMode = %v, want bypassPermissions (auto-approves Agent/Workflow headless)", perms["defaultMode"])
	}
	// With an empty allow-list, NO allow key is emitted, so nothing can exclude
	// Agent/Workflow. (A present-but-partial allow list under bypass is still
	// fully permissive, but we assert absence to catch an accidental restriction.)
	if _, present := perms["allow"]; present {
		t.Fatalf("allow key must be absent under empty allow-list, got %v", perms["allow"])
	}
}

func TestWriteSettings_AttributionDisabled(t *testing.T) {
	home := t.TempDir()
	if err := writeSettings(Params{HookCommand: "/x"}, home); err != nil {
		t.Fatalf("writeSettings: %v", err)
	}
	m := readSettings(t, home)
	attr, ok := m["attribution"].(map[string]any)
	if !ok {
		t.Fatal("attribution missing")
	}
	if attr["commit"] != "" || attr["pr"] != "" || attr["sessionUrl"] != false {
		t.Errorf("attribution not disabled: %#v", attr)
	}
}

func TestWriteSettings_AllowListNeverDropsDispatchTools(t *testing.T) {
	// If a non-empty allow-list is ever supplied, it must include the dispatch
	// tools (Agent, Workflow) so a future switch away from bypassPermissions
	// would not hang a headless turn on subagent/workflow approval.
	home := t.TempDir()
	allow := []string{"Bash", "Edit", "Agent", "Workflow"}
	if err := writeSettings(Params{HookCommand: "/x", PermissionMode: "bypassPermissions", AllowedTools: allow}, home); err != nil {
		t.Fatalf("writeSettings: %v", err)
	}
	m := readSettings(t, home)
	perms := m["permissions"].(map[string]any)
	got, _ := perms["allow"].([]any)
	var asStr []string
	for _, v := range got {
		asStr = append(asStr, v.(string))
	}
	for _, want := range []string{"Agent", "Workflow"} {
		found := false
		for _, a := range asStr {
			if a == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("dispatch tool %q missing from allow-list %v", want, asStr)
		}
	}
}
