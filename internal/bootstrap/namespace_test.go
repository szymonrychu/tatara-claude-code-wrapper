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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := namespacePath(tc.url); got != tc.want {
				t.Fatalf("namespacePath(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}
