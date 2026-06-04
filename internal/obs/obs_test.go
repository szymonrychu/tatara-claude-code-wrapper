package obs_test

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/obs"
)

func TestNew_AssemblesAll(t *testing.T) {
	var buf bytes.Buffer
	o := obs.New(&buf, slog.LevelInfo)
	require.NotNil(t, o.Logger)
	require.NotNil(t, o.Registry)
}
