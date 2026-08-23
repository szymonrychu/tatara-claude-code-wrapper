package promptguard_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/promptguard"
)

func TestManifestToolNames_DecodesAndSorts(t *testing.T) {
	got, err := promptguard.ManifestToolNames(strings.NewReader(
		`{"tools":[{"name":"submit_outcome"},{"name":"issue_write","enums":{"action":["comment"]}}]}`))
	require.NoError(t, err)
	require.Equal(t, []string{"issue_write", "submit_outcome"}, got)
}

// The three fail-closed shapes. Each of them is a manifest fetch that produced
// something the guard could read as "no tool is missing".
//
// This is deliberately NOT tatara-agent-skills' validate_tool_calls.py:226-231,
// which returns 0 with a WARNING on a manifest with no tools against its own
// fail-closed docstring. It is promptguard.Literals's shape: an empty scan is
// an error that says so.
func TestManifestToolNames_FailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"no tools key", `{}`},
		{"empty tools array", `{"tools":[]}`},
		{"every entry unnamed", `{"tools":[{"enums":{}},{"enums":{}}]}`},
		{"html error page instead of json", `<!DOCTYPE html><html>Not Found</html>`},
		{"empty body", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := promptguard.ManifestToolNames(strings.NewReader(tc.body))
			require.Error(t, err)
		})
	}
}

// A named entry among unnamed ones still counts; the guard must not silently
// drop entries it could not read a name from either.
func TestManifestToolNames_RejectsAnUnnamedEntry(t *testing.T) {
	_, err := promptguard.ManifestToolNames(strings.NewReader(
		`{"tools":[{"name":"issue_write"},{"enums":{}}]}`))
	require.Error(t, err)
}
