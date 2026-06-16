package bootstrap

import "testing"

func TestNamespacePath(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"https github with .git", "https://github.com/szymonrychu/tatara-cli.git", "szymonrychu/tatara-cli"},
		{"https github no .git", "https://github.com/szymonrychu/tatara-cli", "szymonrychu/tatara-cli"},
		{"https gitlab subgroups", "https://gitlab.com/szymonrychu/infra/helmfile", "szymonrychu/infra/helmfile"},
		{"https gitlab subgroups with .git", "https://gitlab.com/szymonrychu/infra/helmfile.git", "szymonrychu/infra/helmfile"},
		{"scp-like ssh with .git", "git@github.com:szymonrychu/tatara-cli.git", "szymonrychu/tatara-cli"},
		{"scp-like ssh no .git", "git@github.com:szymonrychu/tatara-cli", "szymonrychu/tatara-cli"},
		{"ssh scheme with port and subgroup", "ssh://git@host:22/group/sub/repo.git", "group/sub/repo"},
		{"https with userinfo", "https://x-access-token:tok@github.com/szymonrychu/tatara-cli.git", "szymonrychu/tatara-cli"},
		{"trailing slash", "https://github.com/szymonrychu/tatara-cli/", "szymonrychu/tatara-cli"},
		{"http scheme", "http://example.com/owner/repo.git", "owner/repo"},
		// finding 10: malformed URLs with no owner segment must return ""
		// so callers (Render, CommitAndPushAll) treat them as invalid and skip/error.
		// host-only URL: after stripping scheme + host, nothing remains.
		{"host only", "https://github.com/", ""},
		// empty URL
		{"empty url", "", ""},
		// finding 2: dotless host with scheme present - host must be stripped
		// unconditionally (not via dot/colon heuristic). After stripping "host",
		// only "repo" remains (single segment, no owner) -> return "".
		{"host no dot scheme present", "https://host/repo.git", ""},
		// dotless host with owner/repo: after stripping host "gitea" we get
		// owner/repo which is valid.
		{"dotless host with owner repo", "https://gitea/owner/repo.git", "owner/repo"},
		// scp-like with dotless host (no scheme): host stripping goes via colon
		// split, not the hasSchemeSep path -> owner/repo unchanged.
		{"scp dotless host", "git@gitea:owner/repo.git", "owner/repo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := namespacePath(tc.url); got != tc.want {
				t.Fatalf("namespacePath(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}
