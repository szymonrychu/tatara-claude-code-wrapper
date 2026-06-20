package bootstrap

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPluginConfig_Empty(t *testing.T) {
	m, e := pluginConfig(nil)
	require.Nil(t, m)
	require.Nil(t, e)
}

func TestPluginConfig_GitHubMarketplace(t *testing.T) {
	m, e := pluginConfig([]PluginSpec{{Name: "formatter@acme", Source: "acme/plugins"}})
	require.True(t, e["formatter@acme"])
	km, ok := m["acme"]
	require.True(t, ok, "marketplace keyed by the @suffix of Name")
	require.Equal(t, "github", km.Source.Source)
	require.Equal(t, "acme/plugins", km.Source.Repo)
	require.Empty(t, km.Source.URL)
}

func TestPluginConfig_GitURLMarketplace(t *testing.T) {
	m, e := pluginConfig([]PluginSpec{{Name: "tool@gl", Source: "https://gitlab.com/co/plugins.git"}})
	require.True(t, e["tool@gl"])
	require.Equal(t, "git", m["gl"].Source.Source)
	require.Equal(t, "https://gitlab.com/co/plugins.git", m["gl"].Source.URL)
	require.Empty(t, m["gl"].Source.Repo)
}

func TestPluginConfig_NoSourceEnablesOnly(t *testing.T) {
	m, e := pluginConfig([]PluginSpec{{Name: "builtin@official"}})
	require.True(t, e["builtin@official"])
	require.Empty(t, m, "no marketplace registered without a Source")
}

func TestPluginConfig_SourceWithoutAtSuffixSkipsMarketplace(t *testing.T) {
	// A Source but no @marketplace suffix: cannot bind the source to a key, so
	// the plugin is still enabled by name but no marketplace is registered.
	m, e := pluginConfig([]PluginSpec{{Name: "lonely", Source: "acme/plugins"}})
	require.True(t, e["lonely"])
	require.Empty(t, m)
}

func TestWriteSettings_ExtraSettingsMergedButOperatorKeysWin(t *testing.T) {
	home := t.TempDir()
	p := Params{
		HookCommand:   "/x",
		Effort:        "xhigh",
		ExtraSettings: []byte(`{"maxParallelism":4,"effortLevel":"low","statusLine":"x"}`),
	}
	require.NoError(t, writeSettings(p, home))
	m := readSettings(t, home)

	// Non-managed extra key passes through.
	require.EqualValues(t, 4, m["maxParallelism"])
	require.Equal(t, "x", m["statusLine"])
	// Operator-managed effortLevel must win over the extra value.
	require.Equal(t, "xhigh", m["effortLevel"])
}

func TestWriteSettings_ExtraCannotClobberPermissions(t *testing.T) {
	home := t.TempDir()
	p := Params{
		HookCommand:    "/x",
		PermissionMode: "bypassPermissions",
		ExtraSettings:  []byte(`{"permissions":{"deny":[]}}`),
	}
	require.NoError(t, writeSettings(p, home))
	m := readSettings(t, home)
	perms := m["permissions"].(map[string]any)
	deny := perms["deny"].([]any)
	require.Contains(t, deny, "AskUserQuestion", "operator deny list must survive an extra-settings clobber attempt")
	require.Equal(t, "bypassPermissions", perms["defaultMode"])
}

func TestWriteSettings_InvalidExtraSettingsErrors(t *testing.T) {
	home := t.TempDir()
	err := writeSettings(Params{HookCommand: "/x", ExtraSettings: []byte(`not json`)}, home)
	require.Error(t, err)
}

func TestWriteSettings_PluginsRendered(t *testing.T) {
	home := t.TempDir()
	p := Params{
		HookCommand: "/x",
		Plugins:     []PluginSpec{{Name: "formatter@acme", Source: "acme/plugins"}},
	}
	require.NoError(t, writeSettings(p, home))
	m := readSettings(t, home)

	enabled := m["enabledPlugins"].(map[string]any)
	require.Equal(t, true, enabled["formatter@acme"])
	markets := m["extraKnownMarketplaces"].(map[string]any)
	require.Contains(t, markets, "acme")
}

func TestWriteSettings_NoPluginKeysWhenAbsent(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, writeSettings(Params{HookCommand: "/x"}, home))
	m := readSettings(t, home)
	_, hasEnabled := m["enabledPlugins"]
	_, hasMarkets := m["extraKnownMarketplaces"]
	require.False(t, hasEnabled)
	require.False(t, hasMarkets)
}
