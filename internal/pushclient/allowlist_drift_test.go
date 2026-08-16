package pushclient

import (
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
)

// descFQName pulls the fully-qualified metric name out of a Desc. Desc has no
// accessor for it; String() is the only exported surface that carries it.
var descFQName = regexp.MustCompile(`fqName: "([^"]+)"`)

// descCapture is a prometheus.Registerer that records the fqName of every Desc
// a collector describes, instead of registering it. Describe (not Gather) is
// the right source here: a CounterVec with no child yet gathers to nothing, so
// a gather-based check would silently skip exactly the label-carrying families
// most likely to drift.
type descCapture struct{ names []string }

func (d *descCapture) Register(c prometheus.Collector) error {
	ch := make(chan *prometheus.Desc, 128)
	go func() {
		c.Describe(ch)
		close(ch)
	}()
	for desc := range ch {
		if m := descFQName.FindStringSubmatch(desc.String()); m != nil {
			d.names = append(d.names, m[1])
		}
	}
	return nil
}

func (d *descCapture) MustRegister(cs ...prometheus.Collector) {
	for _, c := range cs {
		_ = d.Register(c)
	}
}

func (d *descCapture) Unregister(prometheus.Collector) bool { return false }

// TestEveryRegisteredFamilyIsPushAllowed is the drift guard for issue #173.
//
// Agent pods have no scrape target: a wrapper-owned family that allowedFamily
// rejects is discarded inside the pod and never reaches Prometheus at all, and
// because allowlisted() filters ahead of the wire the operator's
// operator_push_series_dropped_total cannot witness the drop either. Three
// families (wrapper_skills_installed_total, wrapper_skills_clone_failures_total,
// wrapper_agents_installed_total) shipped that way and stayed dark, green in CI
// the whole time because every unit test gathers the local registry directly
// and so bypasses allowedFamily.
//
// This asserts the invariant instead of the three names: everything
// metrics.New registers must be push-allowed. Adding a family under a fresh
// prefix now fails the build rather than going dark in production.
func TestEveryRegisteredFamilyIsPushAllowed(t *testing.T) {
	cap := &descCapture{}
	metrics.New(cap)
	require.NotEmpty(t, cap.names, "metrics.New must register collectors")

	var dark []string
	for _, name := range cap.names {
		// The Go/process runtime collectors are added by obs.PromRegistry, not
		// by metrics.New, and are deliberately excluded from the push (issue
		// #59). Nothing metrics.New registers may carry those prefixes.
		require.False(t, strings.HasPrefix(name, "go_") || strings.HasPrefix(name, "process_"),
			"metrics.New must not register a runtime-collector name: %s", name)
		if !allowedFamily(name) {
			dark = append(dark, name)
		}
	}
	require.Empty(t, dark,
		"these families are registered but not push-allowed, so they can never reach Prometheus "+
			"from an agent pod: %v (fix the name to a pushAllowedPrefixes prefix, or widen the prefix list)", dark)
}
