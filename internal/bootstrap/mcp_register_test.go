package bootstrap_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
)

func TestRegisterTataraMCP_RunsMcpConfig(t *testing.T) {
	var calls [][]string
	run := func(name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return nil
	}
	require.NoError(t, bootstrap.RegisterTataraMCP("/workspace", run))
	require.Len(t, calls, 1)
	require.Equal(t, []string{"tatara", "mcp-config", "/workspace"}, calls[0])
}

func TestRegisterTataraMCP_PropagatesError(t *testing.T) {
	run := func(name string, args ...string) error {
		return errors.New("tatara not found")
	}
	err := bootstrap.RegisterTataraMCP("/workspace", run)
	require.Error(t, err)
	require.Contains(t, err.Error(), "tatara not found")
}
