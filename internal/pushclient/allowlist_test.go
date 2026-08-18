package pushclient

import (
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
)

// capturingRegisterer records what metrics.New registers without needing a real
// registry. A registry would only surface families that have already been
// observed, which hides every unused CounterVec - exactly the families that go
// dark in production while their unit tests stay green (issue #173).
type capturingRegisterer struct{ collectors []prometheus.Collector }

func (c *capturingRegisterer) Register(col prometheus.Collector) error {
	c.collectors = append(c.collectors, col)
	return nil
}

func (c *capturingRegisterer) MustRegister(cols ...prometheus.Collector) {
	c.collectors = append(c.collectors, cols...)
}

func (c *capturingRegisterer) Unregister(prometheus.Collector) bool { return false }

var fqNameRE = regexp.MustCompile(`fqName: "([^"]+)"`)

// registeredFamilyNames returns the fully-qualified name of every metric
// metrics.New registers, read off the Desc rather than off a Gather so that
// never-observed vectors are still covered.
func registeredFamilyNames(t *testing.T) []string {
	t.Helper()
	cr := &capturingRegisterer{}
	metrics.New(cr)
	require.NotEmpty(t, cr.collectors, "metrics.New registered nothing")

	var names []string
	for _, col := range cr.collectors {
		ch := make(chan *prometheus.Desc, 16)
		go func() {
			col.Describe(ch)
			close(ch)
		}()
		for d := range ch {
			m := fqNameRE.FindStringSubmatch(d.String())
			require.Len(t, m, 2, "cannot read fqName out of %s", d.String())
			names = append(names, m[1])
		}
	}
	return names
}

// TestPushAllowlist_CoversEveryRegisteredFamily is the drift guard for issue
// #173: wrapper_skills_installed_total, wrapper_skills_clone_failures_total and
// wrapper_agents_installed_total were registered, incremented on every boot, and
// silently discarded by allowlisted() before the push - so they never reached
// Prometheus while their own unit tests, which gather from the local registry,
// stayed green. Agent pods have no scrape target, so a family the push filter
// drops does not exist. Registering a wrapper-owned metric outside
// pushAllowedPrefixes must fail the build, not the fleet.
func TestPushAllowlist_CoversEveryRegisteredFamily(t *testing.T) {
	for _, name := range registeredFamilyNames(t) {
		// Runtime collectors are shared with the /metrics endpoint and are
		// dropped by the receiver as reserved_name; they are meant to be filtered.
		if strings.HasPrefix(name, "go_") || strings.HasPrefix(name, "process_") {
			continue
		}
		require.Truef(t, allowedFamily(name),
			"metric %q is registered but not push-allowed: it can never reach Prometheus "+
				"(agent pods have no scrape target). Rename it under one of %v, or widen the list.",
			name, pushAllowedPrefixes)
	}
}
