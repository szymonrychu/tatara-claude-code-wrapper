package bootstrap

import (
	"testing"
)

func TestWriteSettingsStatusLine(t *testing.T) {
	tests := []struct {
		name    string
		command string
		wantKey bool
	}{
		{name: "configured command renders statusLine", command: "/usr/local/bin/cc-statusline", wantKey: true},
		{name: "empty command renders no statusLine key", command: "", wantKey: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if err := writeSettings(Params{
				HookCommand:       "/usr/local/bin/cc-stop-hook",
				StatusLineCommand: tc.command,
			}, home); err != nil {
				t.Fatalf("writeSettings: %v", err)
			}
			doc := readSettings(t, home)
			sl, ok := doc["statusLine"].(map[string]any)
			if ok != tc.wantKey {
				t.Fatalf("statusLine present = %v, want %v", ok, tc.wantKey)
			}
			if tc.wantKey {
				if sl["type"] != "command" {
					t.Fatalf("statusLine.type = %v, want command", sl["type"])
				}
				if sl["command"] != tc.command {
					t.Fatalf("statusLine.command = %v, want %v", sl["command"], tc.command)
				}
			}
			// The Stop hook must be untouched by this change.
			hooks, ok := doc["hooks"].(map[string]any)
			if !ok {
				t.Fatalf("hooks block missing")
			}
			if _, ok := hooks["Stop"]; !ok {
				t.Fatalf("hooks.Stop missing")
			}
		})
	}
}
