package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/webhook"
)

// A FAILED PUSH MUST REACH THE OPERATOR, NOT JUST THE POD LOG.
//
// The callback deliberately still fires when commit/push fails, and it used to
// carry only pushedRepos - so the operator's write-back read a short list as a
// complete turn and stampLastTurn recorded a truthful-looking partial. Now that
// the loop no longer aborts on the first failing repo, that blind spot would
// convert one loud failure into N quiet partial successes.
//
// Single-repo path: the workspace holds no clone, so `git add -A` fails, and the
// one repo this pod is bound to must be named on the wire.
func TestFinalizeTurn_ReportsTheRepoThatFailedToPush(t *testing.T) {
	var delivered map[string]any
	got := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&delivered)
		close(got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := config{
		Workspace:  t.TempDir(), // no clone in it: the first git call fails
		RepoURL:    "https://github.com/szymonrychu/tatara-cli.git",
		TaskBranch: "tatara/task-x",
	}
	rec := &turn.Record{ID: "turn-1", CallbackURL: srv.URL}
	a := &app{log: testLogger(), sess: newTestSession(t)}
	a.finalizeTurn(rec, cfg, metrics.New(prometheus.NewRegistry()), testLogger(),
		webhook.New(webhook.Config{Retries: 1}, metrics.New(prometheus.NewRegistry()), testLogger()), "")

	require.Equal(t, []string{"tatara-cli"}, rec.FailedRepos,
		"the turn record must name the repo whose work never reached origin")

	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("callback never delivered")
	}
	require.Equal(t, []any{"tatara-cli"}, delivered["failedRepos"],
		"failedRepos is a wire field: the operator cannot see a short push list any other way")
}

// The happy path must stay silent. failedRepos is omitempty precisely so an
// ordinary turn's payload is byte-identical to what it was before.
func TestFinalizeTurn_NoFailedReposOnTheWireWhenNothingFailed(t *testing.T) {
	var raw map[string]any
	got := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		close(got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rec := &turn.Record{ID: "turn-1", CallbackURL: srv.URL}
	a := &app{log: testLogger(), sess: newTestSession(t)}
	a.finalizeTurn(rec, config{}, metrics.New(prometheus.NewRegistry()), testLogger(),
		webhook.New(webhook.Config{Retries: 1}, metrics.New(prometheus.NewRegistry()), testLogger()), "")

	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("callback never delivered")
	}
	require.NotContains(t, raw, "failedRepos")
}

func newTestSession(t *testing.T) *session.Manager {
	t.Helper()
	return session.New(session.Config{}, turn.NewStore(), metrics.New(prometheus.NewRegistry()),
		testLogger(), time.Now, func() string { return "turn-1" })
}

// A BLANK NAME IS NOT A REPORT, AND ONE BLANK ELEMENT DOES REAL DAMAGE.
//
// primaryRepoName returns "" when it has nothing to name, rather than falling
// back to cfg.RepoURL (unvalidated, and empty on a project-scoped pod: the
// operator omits REPO_URL when the Task has no Repository). A
// `failedRepos: [""]` would name nothing while still being len()==1 on the
// operator side, which is enough to make a content-free turn compute
// contentFree=false - suppressing the placeholder note and the
// empty-synthetic counter that exist to catch exactly that loss.
func TestFinalizeTurn_NeverReportsABlankFailedRepoName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// No RepoURL and no Repos: RepoDir returns "", so the single-repo path errors
	// before it can name anything.
	cfg := config{Workspace: t.TempDir(), TaskBranch: "tatara/task-x"}
	rec := &turn.Record{ID: "turn-1", CallbackURL: srv.URL}
	a := &app{log: testLogger(), sess: newTestSession(t)}
	a.finalizeTurn(rec, cfg, metrics.New(prometheus.NewRegistry()), testLogger(),
		webhook.New(webhook.Config{Retries: 1}, metrics.New(prometheus.NewRegistry()), testLogger()), "")

	require.Empty(t, rec.FailedRepos,
		"an unnameable repo must report nothing rather than an empty string")
}

// primaryRepoName must never hand a raw URL to a caller expecting a repo
// name: a malformed REPO_URL is exactly the case bootstrap.RepoDir already
// signals with "", and the old cfg.RepoURL fallback defeated that signal by
// reporting the URL itself instead of nothing.
func TestPrimaryRepoName(t *testing.T) {
	t.Run("malformed REPO_URL yields no name", func(t *testing.T) {
		cfg := config{Workspace: t.TempDir(), RepoURL: "https://github.com/"}
		require.Empty(t, bootstrap.RepoDir(cfg.Workspace, cfg.RepoURL),
			"test premise: RepoDir must genuinely fail to derive a namespace for this URL")
		require.Empty(t, primaryRepoName(cfg),
			"a malformed REPO_URL must report nothing, not the raw URL")
	})

	t.Run("normal URL yields the base name", func(t *testing.T) {
		cfg := config{Workspace: t.TempDir(), RepoURL: "https://github.com/szymonrychu/tatara-cli.git"}
		require.Equal(t, "tatara-cli", primaryRepoName(cfg))
	})
}
