package obs_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/obs"
)

func TestPromRegistry_HasDefaultCollectors(t *testing.T) {
	reg := obs.PromRegistry()
	require.NotNil(t, reg)

	mfs, err := reg.Gather()
	require.NoError(t, err)

	names := map[string]bool{}
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	require.True(t, names["go_goroutines"], "expected go collector")
	require.True(t, names["process_cpu_seconds_total"], "expected process collector")
}
