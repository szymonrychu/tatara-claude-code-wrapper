package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
)

// The families that witness a failed boot - ccw_skills_clone_failures_total,
// ccw_skills_installed_total at 0, ccw_bootstrap_render_total{result="fail"} -
// are incremented inside bootstrap.Render, which newApp calls BEFORE the push
// loop exists. An agent pod has no scrape target, so without a synchronous push
// on the failure path the fatal case is exactly the case the #173 rename cannot
// see. renderAndPushOnFailure is the seam newApp uses.

type pushRecorder struct {
	mu   sync.Mutex
	body string
	hits int
}

func (p *pushRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPost {
			p.mu.Lock()
			p.body = string(b)
			p.hits++
			p.mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// bootCfg is the minimal config that drives Render: a writable home and
// workspace, and a skills source directory the caller controls.
func bootCfg(t *testing.T, srv *httptest.Server, skillsSrc string) config {
	t.Helper()
	return config{
		HomeDir:             t.TempDir(),
		Workspace:           t.TempDir(),
		HookPath:            "/usr/local/bin/cc-stop-hook",
		PermissionMode:      "bypassPermissions",
		SkillsSrcDirs:       skillsSrc,
		OperatorPushURL:     srv.URL + "/internal/metrics/push",
		RunID:               "run-boot",
		PodName:             "pod-boot",
		PushIntervalSeconds: 3600,
	}
}

func TestRenderAndPushOnFailure_PushesTheWitnessMetrics(t *testing.T) {
	rec := &pushRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	// An existing but empty skills source: Render installs 0 skills and fails.
	cfg := bootCfg(t, srv, filepath.Join(t.TempDir(), "skills", "skills"))
	require.NoError(t, os.MkdirAll(cfg.SkillsSrcDirs, 0o755))

	reg := prometheus.NewRegistry()

	err := renderAndPushOnFailure(context.Background(), cfg, reg, testLogger(), metrics.New(reg),
		func(_ string, _ ...string) error { return nil })
	require.Error(t, err, "an empty skills source must fail the boot")

	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Equal(t, 1, rec.hits, "the fatal boot path must push before returning")
	require.Contains(t, rec.body, `ccw_bootstrap_render_total`)
	require.Contains(t, rec.body, `result="fail"`)
	require.Contains(t, rec.body, "ccw_skills_installed_total")
}

func TestRenderAndPushOnFailure_SuccessDoesNotPush(t *testing.T) {
	rec := &pushRecorder{}
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	// A source carrying one skill: Render succeeds, and the normal push loop
	// (started later in run()) owns the reporting from there.
	src := filepath.Join(t.TempDir(), "skills", "skills")
	require.NoError(t, os.MkdirAll(filepath.Join(src, "my-skill"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "my-skill", "SKILL.md"),
		[]byte("---\nname: my-skill\n---\n"), 0o644))

	cfg := bootCfg(t, srv, src)
	reg := prometheus.NewRegistry()
	require.NoError(t, renderAndPushOnFailure(context.Background(), cfg, reg,
		testLogger(), metrics.New(reg), func(_ string, _ ...string) error { return nil }))

	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Zero(t, rec.hits, "a successful boot must not push out of band")
}
