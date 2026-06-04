package auth_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/auth"
)

func TestConfig_Validate(t *testing.T) {
	require.Error(t, (auth.Config{}).Validate())
	require.Error(t, (auth.Config{Issuer: "https://example/realms/master"}).Validate())
	require.NoError(t, (auth.Config{
		Issuer:   "https://example/realms/master",
		Audience: "tatara-memory",
	}).Validate())
}
