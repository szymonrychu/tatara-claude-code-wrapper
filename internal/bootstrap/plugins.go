package bootstrap

import "strings"

// PluginSpec is one claude-code plugin the operator asked to install. It mirrors
// one entry of the operator-mounted plugins.json (the operator's api/v1alpha1
// Plugin type). Name is the plugin identifier used in settings.json
// enabledPlugins, conventionally "plugin@marketplace"; Source, when set, is the
// marketplace to register (a GitHub "owner/repo" or a git URL) under the
// marketplace name taken from the "@marketplace" suffix of Name.
type PluginSpec struct {
	Name   string `json:"name"`
	Source string `json:"source,omitempty"`
}

// marketplaceSource is the settings.json extraKnownMarketplaces[*].source shape.
type marketplaceSource struct {
	Source string `json:"source"`         // "github" or "git"
	Repo   string `json:"repo,omitempty"` // source=github: "owner/repo"
	URL    string `json:"url,omitempty"`  // source=git: clone URL
}

type knownMarketplace struct {
	Source marketplaceSource `json:"source"`
}

// pluginConfig translates the plugin list into the two declarative settings.json
// keys Claude Code reads at startup: extraKnownMarketplaces and enabledPlugins.
// This is the headless install path: the interactive /plugin commands have no
// non-interactive flag, but plugins listed in enabledPlugins (with their
// marketplace registered in extraKnownMarketplaces) load at boot without a TTY.
// Returns nil maps when there is nothing to install so the keys are omitted.
func pluginConfig(plugins []PluginSpec) (map[string]knownMarketplace, map[string]bool) {
	if len(plugins) == 0 {
		return nil, nil
	}
	markets := map[string]knownMarketplace{}
	enabled := map[string]bool{}
	for _, pl := range plugins {
		if pl.Name == "" {
			continue
		}
		enabled[pl.Name] = true
		if pl.Source == "" {
			// No marketplace to register: the plugin is expected to come from a
			// marketplace already known to claude (or be a built-in id).
			continue
		}
		// Register the marketplace named in the plugin id's "@marketplace"
		// suffix, so the extraKnownMarketplaces key matches the enabledPlugins
		// reference. A Source without that suffix cannot be bound to a key, so
		// the plugin stays enabled by name but the source is skipped.
		at := strings.LastIndex(pl.Name, "@")
		if at < 0 || at == len(pl.Name)-1 {
			continue
		}
		mkt := pl.Name[at+1:]
		markets[mkt] = knownMarketplace{Source: marketplaceSourceOf(pl.Source)}
	}
	if len(enabled) == 0 {
		return nil, nil
	}
	return markets, enabled
}

// marketplaceSourceOf classifies a marketplace source string: a value with a
// scheme ("://") or an scp-style "git@" prefix is a git URL, anything else is a
// GitHub "owner/repo".
func marketplaceSourceOf(source string) marketplaceSource {
	if strings.Contains(source, "://") || strings.HasPrefix(source, "git@") {
		return marketplaceSource{Source: "git", URL: source}
	}
	return marketplaceSource{Source: "github", Repo: source}
}
