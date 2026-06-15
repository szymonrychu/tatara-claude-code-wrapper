package bootstrap

import "strings"

// namespacePath maps a git clone URL to the on-disk subpath:
// owner[/subgroups]/repo, dropping scheme, host, userinfo, and a trailing
// ".git". It keeps the owner and any subgroups.
//
//	https://github.com/szymonrychu/tatara-cli.git -> szymonrychu/tatara-cli
//	https://gitlab.com/szymonrychu/infra/helmfile  -> szymonrychu/infra/helmfile
//	git@github.com:szymonrychu/tatara-cli.git      -> szymonrychu/tatara-cli
//	ssh://git@host:22/group/sub/repo.git           -> group/sub/repo
func namespacePath(cloneURL string) string {
	s := strings.TrimSpace(cloneURL)
	// Detect scp-like syntax (no scheme) before stripping anything.
	// scp-like: git@github.com:owner/repo(.git) - no "://" present.
	hasSchemeSep := strings.Contains(s, "://")
	// Strip a URL scheme (https://, http://, ssh://, git://).
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	// scp-like syntax: git@github.com:owner/repo(.git). Only applies when the
	// original URL had no "://" (i.e. it truly is scp-form, not ssh://host:port/path).
	if !hasSchemeSep && !strings.HasPrefix(s, "/") {
		if i := strings.Index(s, ":"); i >= 0 {
			if slash := strings.Index(s, "/"); slash < 0 || i < slash {
				s = s[i+1:]
			}
		}
	}
	// Drop userinfo (user@) if present at the front of host[:port]/path.
	if i := strings.Index(s, "@"); i >= 0 {
		if slash := strings.Index(s, "/"); slash < 0 || i < slash {
			s = s[i+1:]
		}
	}
	s = strings.Trim(s, "/")
	// Now s is host[:port]/owner[/subgroups]/repo OR owner[/subgroups]/repo.
	parts := strings.Split(s, "/")
	// If the first segment looks like a host (contains '.' or ':'), drop it.
	if len(parts) > 1 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":")) {
		parts = parts[1:]
	}
	out := strings.Join(parts, "/")
	out = strings.TrimSuffix(out, ".git")
	// Require at least owner/repo shape. A single-segment result has no owner
	// and would clone into a bare directory (e.g. ws/github.com or ws/repo).
	// Return "" so callers treat it the same as an empty URL and skip/error.
	if !strings.Contains(out, "/") {
		return ""
	}
	return out
}
