package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRing_ContainsIgnoresAnsiAndWhitespace(t *testing.T) {
	r := newRing()
	// Mimic the TUI: words separated by cursor-move escapes (no literal spaces),
	// wrapped in color codes.
	_, _ = r.Write([]byte("\x1b[1mWARNING:\x1b[2GBypass\x1b[9GPermissions\x1b[21Gmode\x1b[0m"))
	require.True(t, r.contains("Bypass Permissions mode"))
	require.True(t, r.contains("BypassPermissionsmode"))
	require.False(t, r.contains("Trust this folder"))
}

func TestRing_TailStripsAnsiAndCaps(t *testing.T) {
	r := newRing()
	_, _ = r.Write([]byte("\x1b[31mhello\x1b[0m world"))
	require.Equal(t, "hello world", r.tail(100))

	r2 := newRing()
	_, _ = r2.Write([]byte(strings.Repeat("x", ringCap+1000)))
	require.LessOrEqual(t, len(r2.tail(ringCap*2)), ringCap)
	require.Greater(t, r2.written(), ringCap) // monotonic count keeps growing
}

func TestRing_ResetClearsBufferKeepsTotal(t *testing.T) {
	r := newRing()
	_, _ = r.Write([]byte("Bypass Permissions mode"))
	require.True(t, r.contains("Bypass Permissions mode"))
	before := r.written()

	r.reset()

	// Buffered dialog text is gone, so a relaunch re-detects from scratch...
	require.False(t, r.contains("Bypass Permissions mode"))
	require.Equal(t, "", r.tail(100))
	// ...but the monotonic counter is preserved so bootWait's written() baseline
	// keeps working across the relaunch.
	require.Equal(t, before, r.written())
}
