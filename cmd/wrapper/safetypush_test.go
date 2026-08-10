package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

// recordingGit collects every git invocation the safety net makes, safely
// under -race (the ticker runs on its own goroutine).
type recordingGit struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *recordingGit) runner() bootstrap.GitRunner {
	return func(dir string, args ...string) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls = append(r.calls, append([]string{dir}, args...))
		return nil
	}
}

func (r *recordingGit) snapshot() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	copy(out, r.calls)
	return out
}

func newTestSafetyPusher(cfg config, git bootstrap.GitRunner) *safetyPusher {
	return newSafetyPusher(cfg, git, testLogger(), metrics.New(prometheus.NewRegistry()))
}

// A Review Task checks its branch out read-only via CheckoutBranch and must
// NEVER push: the branch belongs to someone else's pull request. The guard
// mirrors finalizeTurn's own `cfg.TaskBranch != ""`.
func TestSafetyPusher_NoOpWithoutTaskBranch(t *testing.T) {
	g := &recordingGit{}
	cfg := config{
		Workspace:                 "/tmp/ws",
		RepoURL:                   "https://github.com/owner/repo",
		CheckoutBranch:            "pr-head",
		BranchPushIntervalSeconds: 1,
	}
	sp := newTestSafetyPusher(cfg, g.runner())

	require.False(t, sp.Enabled(), "no task branch means the safety net is off")
	sp.Start()
	sp.pushOnce()
	time.Sleep(50 * time.Millisecond)
	sp.Shutdown(context.Background())

	require.Empty(t, g.snapshot(), "a read-only checkout must never be pushed")
}

// A zero interval is the explicit off switch.
func TestSafetyPusher_ZeroIntervalDisables(t *testing.T) {
	g := &recordingGit{}
	cfg := config{
		Workspace:                 "/tmp/ws",
		RepoURL:                   "https://github.com/owner/repo",
		TaskBranch:                "tatara/feat-1",
		BranchPushIntervalSeconds: 0,
	}
	sp := newTestSafetyPusher(cfg, g.runner())

	require.False(t, sp.Enabled())
	sp.Start()
	time.Sleep(50 * time.Millisecond)
	sp.Shutdown(context.Background())

	require.Empty(t, g.snapshot())
}

// Every repo carrying the task branch gets pushed, in the same
// workspace/<owner>/<repo> shape CommitAndPushAll iterates.
func TestSafetyPusher_PushesEveryRepoAndNeverTouchesTheIndex(t *testing.T) {
	g := &recordingGit{}
	cfg := config{
		Workspace:  "/tmp/ws",
		TaskBranch: "tatara/feat-1",
		Repos: []bootstrap.RepoSpec{
			{Name: "a", URL: "https://github.com/owner/a"},
			{Name: "b", URL: "https://github.com/owner/b"},
			{Name: "bad", URL: ""},
		},
		BranchPushIntervalSeconds: 120,
	}
	sp := newTestSafetyPusher(cfg, g.runner())
	require.True(t, sp.Enabled())
	sp.pushOnce()

	calls := g.snapshot()
	require.Len(t, calls, 2, "the empty-namespace repo must be skipped, never pushed from the workspace root")
	require.Equal(t, filepath.Join("/tmp/ws", "owner", "a"), calls[0][0])
	require.Equal(t, filepath.Join("/tmp/ws", "owner", "b"), calls[1][0])
	for _, c := range calls {
		require.Equal(t, []string{"push", "--no-verify", "--quiet", "origin", "tatara/feat-1"}, c[1:])
		require.NotContains(t, c, "add")
		require.NotContains(t, c, "commit")
	}
}

// Single-repo mode targets workspace/<owner>/<repo>, not the workspace root.
func TestSafetyPusher_SingleRepoTargetsCloneSubdir(t *testing.T) {
	g := &recordingGit{}
	cfg := config{
		Workspace:                 "/tmp/ws",
		RepoURL:                   "https://github.com/owner/myrepo",
		TaskBranch:                "tatara/feat-1",
		BranchPushIntervalSeconds: 120,
	}
	newTestSafetyPusher(cfg, g.runner()).pushOnce()

	calls := g.snapshot()
	require.Len(t, calls, 1)
	require.Equal(t, filepath.Join("/tmp/ws", "owner", "myrepo"), calls[0][0])
}

// rec.PushedRepos means "this turn produced a commit" and is consumed by the
// operator (LastTurnPushedRepos drives handoff-note synthesis). The safety net
// pushes commits the agent made at some unknown earlier point, so writing that
// field would be a lie the operator acts on. It must leave turn records alone.
func TestSafetyPusher_DoesNotTouchTurnRecords(t *testing.T) {
	store := turn.NewStore()
	rec := store.Create("turn-1", "do the thing", "", time.Now())
	require.Nil(t, rec.PushedRepos)

	g := &recordingGit{}
	cfg := config{
		Workspace:                 "/tmp/ws",
		RepoURL:                   "https://github.com/owner/myrepo",
		TaskBranch:                "tatara/feat-1",
		BranchPushIntervalSeconds: 120,
	}
	sp := newTestSafetyPusher(cfg, g.runner())
	sp.pushOnce()
	sp.pushOnce()

	require.NotEmpty(t, g.snapshot(), "the pushes must actually have happened")

	got, ok := store.Get("turn-1")
	require.True(t, ok)
	require.Nil(t, got.PushedRepos,
		"only finalizeTurn may write PushedRepos; the safety net must never claim the turn pushed")
}

// The ticker fires repeatedly and stops cleanly on Shutdown.
func TestSafetyPusher_TicksAndStopsOnShutdown(t *testing.T) {
	g := &recordingGit{}
	cfg := config{
		Workspace:  "/tmp/ws",
		RepoURL:    "https://github.com/owner/myrepo",
		TaskBranch: "tatara/feat-1",
	}
	sp := newTestSafetyPusher(cfg, g.runner())
	sp.interval = 10 * time.Millisecond
	sp.Start()

	require.Eventually(t, func() bool { return len(g.snapshot()) >= 3 },
		2*time.Second, 10*time.Millisecond)

	sp.Shutdown(context.Background())
	settled := len(g.snapshot())
	time.Sleep(60 * time.Millisecond)
	require.Equal(t, settled, len(g.snapshot()), "the ticker must stop on Shutdown")
}

// A push failure is a warning, never fatal: the safety net must survive a
// moved remote, a dead network, or a repo that has no such branch, and keep
// ticking.
func TestSafetyPusher_PushFailureIsNotFatal(t *testing.T) {
	var n int
	git := func(dir string, args ...string) error {
		n++
		return errString("fatal: unable to access remote")
	}
	cfg := config{
		Workspace:                 "/tmp/ws",
		RepoURL:                   "https://github.com/owner/myrepo",
		TaskBranch:                "tatara/feat-1",
		BranchPushIntervalSeconds: 120,
	}
	sp := newTestSafetyPusher(cfg, git)
	require.NotPanics(t, func() { sp.pushOnce(); sp.pushOnce() })
	require.Equal(t, 2, n)
}

// The CLAUDE.md directive and the ticker must agree on every input, or the pod
// tells the agent one thing and does another. Both sides call
// bootstrap.AutoPushEnabled; this pins that they still do, through the real
// buildBootstrapParams -> Render path rather than by inspection.
func TestSafetyPusher_EnabledMatchesTheInjectedDirective(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cfg      config
		expected bool
	}{
		{"both set", config{TaskBranch: "tatara/feat-1", BranchPushIntervalSeconds: 120}, true},
		{"interval disabled", config{TaskBranch: "tatara/feat-1", BranchPushIntervalSeconds: 0}, false},
		{"read-only checkout", config{CheckoutBranch: "pr-head", BranchPushIntervalSeconds: 120}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := newTestSafetyPusher(tc.cfg, (&recordingGit{}).runner())
			require.Equal(t, tc.expected, sp.Enabled())

			p := buildBootstrapParams(tc.cfg, testLogger(), metrics.New(prometheus.NewRegistry()))
			require.Equal(t, tc.expected,
				bootstrap.AutoPushEnabled(p.TaskBranch, p.BranchPushInterval),
				"buildBootstrapParams must carry the interval through so Render gates the directive the same way")
		})
	}
}

type errString string

func (e errString) Error() string { return string(e) }
