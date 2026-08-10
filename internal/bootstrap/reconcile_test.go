package bootstrap_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
)

// injectedGlobalClaudeMd reads the global CLAUDE.md Render writes into the
// agent's home, which is the surface the resumed-branch directive lands on.
func injectedGlobalClaudeMd(t *testing.T, p bootstrap.Params) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(p.HomeDir, ".claude", "CLAUDE.md"))
	require.NoError(t, err)
	return string(b)
}

// script appends a scripted git outcome. Mirrors the anonymous struct literal
// the existing bootstrap tests build inline, so both styles interoperate.
func (s *scriptedGit) script(match func([]string) bool, err error) {
	s.scripts = append(s.scripts, struct {
		match func([]string) bool
		err   error
	}{match, err})
}

// callsEqual returns the recorded calls whose argv is EXACTLY want. Needed
// because callsContainingAll("fetch", "origin") also matches
// "fetch --unshallow origin" and "fetch origin <branch>".
func callsEqual(calls [][]string, want ...string) [][]string {
	var out [][]string
	for _, c := range calls {
		if len(c) != len(want) {
			continue
		}
		match := true
		for i := range c {
			if c[i] != want[i] {
				match = false
				break
			}
		}
		if match {
			out = append(out, c)
		}
	}
	return out
}

func counterValue(t *testing.T, reg *prometheus.Registry, name, resultLabel string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, mt := range mf.GetMetric() {
			if resultLabel == "" {
				return mt.GetCounter().GetValue()
			}
			for _, lp := range mt.GetLabel() {
				if lp.GetName() == "result" && lp.GetValue() == resultLabel {
					return mt.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

func resumeParams(t *testing.T, taskBranch string) bootstrap.Params {
	t.Helper()
	return bootstrap.Params{
		HomeDir: t.TempDir(), Workspace: t.TempDir(),
		BaseMCP:        []byte(`{"mcpServers":{}}`),
		RepoURL:        "https://github.com/x/y",
		RepoBranch:     "main",
		TaskBranch:     taskBranch,
		HookCommand:    "/usr/local/bin/cc-stop-hook",
		PermissionMode: "bypassPermissions",
	}
}

// notAbortMerge matches a `git merge <ref>` that is not `git merge --abort`.
func notAbortMerge(args []string) bool {
	if len(args) == 0 || args[0] != "merge" {
		return false
	}
	for _, a := range args {
		if a == "--abort" {
			return false
		}
	}
	return true
}

// TestResume_StaleBranch_MergesBaseBranch is the core regression for issue
// #131: a resumed task branch that does not already contain origin/<base> must
// be reconciled with it before the agent reads a single file. The reconcile
// MUST be a merge - never a rebase/reset, which would rewrite already-pushed
// history and strand the agent's committed work behind a force-push that is
// not permitted here.
func TestResume_StaleBranch_MergesBaseBranch(t *testing.T) {
	sg := &scriptedGit{}
	taskBranch := "tatara/task-brainstorm-qe-0a25fd8b4164ecd7"
	sg.script(argsContainAll("ls-remote", "--exit-code"), nil)                            // branch exists on origin -> resume
	sg.script(argsContainAll("merge-base", "--is-ancestor"), fmt.Errorf("exit status 1")) // base is NOT an ancestor -> stale

	require.NoError(t, bootstrap.Render(resumeParams(t, taskBranch), sg.run))

	require.NotEmpty(t, callsEqual(sg.Calls, "fetch", "origin"),
		"resume must refresh remote-tracking refs with a plain `git fetch origin` before comparing against the base branch")
	require.NotEmpty(t, callsEqual(sg.Calls, "merge-base", "--is-ancestor", "origin/main", "HEAD"),
		"resume must test whether origin/main is already contained in the resumed branch")
	require.NotEmpty(t, callsContainingAll(sg.Calls, "merge", "origin/main"),
		"a stale resumed branch must be merged with origin/main, not silently used")

	// Nothing may rewrite or discard already-published agent commits.
	require.Empty(t, callsContainingAll(sg.Calls, "rebase"), "must never rebase a pushed task branch")
	require.Empty(t, callsContainingAll(sg.Calls, "reset", "--hard"), "must never hard-reset a resumed task branch")
	require.Empty(t, callsContainingAll(sg.Calls, "--force"), "must never force-push")
	require.Empty(t, callsContainingAll(sg.Calls, "--force-with-lease"), "must never force-push")
	require.Empty(t, callsEqual(sg.Calls, "merge", "--abort"), "a clean merge must not be aborted")
}

// TestResume_UpToDateBranch_NoMerge asserts the reconcile is a no-op when the
// resumed branch already contains the base tip: no empty merge commit is
// created on every turn.
func TestResume_UpToDateBranch_NoMerge(t *testing.T) {
	sg := &scriptedGit{}
	sg.script(argsContainAll("ls-remote", "--exit-code"), nil)
	// merge-base --is-ancestor returns nil (exit 0) by default: already contained.

	reg := prometheus.NewRegistry()
	p := resumeParams(t, "tatara/task-abc")
	p.M = metrics.New(reg)
	require.NoError(t, bootstrap.Render(p, sg.run))

	require.NotEmpty(t, callsEqual(sg.Calls, "merge-base", "--is-ancestor", "origin/main", "HEAD"))
	require.Empty(t, callsContainingAll(sg.Calls, "merge", "origin/main"),
		"an up-to-date branch must not be merged again")
	require.Equal(t, float64(1), counterValue(t, reg, "ccw_bootstrap_reconcile_total", "up_to_date"))
}

// TestResume_UnreconcilableBranch_BecomesTheAgentsFirstJob is the inverse of
// the original contract. A genuine merge conflict on a resumed branch used to
// fail Render, which exited the wrapper non-zero, kept the pod from ever
// becoming Ready and had the operator respawn it every few minutes until the
// residency cap - roughly 288 dead pods per task, and never a single turn.
// A conflict is now a task for the agent, not a boot failure: the merge is
// still aborted (a clean tree at the branch tip, never conflict markers that
// the turn finaliser's `git add -A` would commit), the conflict is still
// counted and still logged at ERROR, Render still succeeds, and the injected
// global CLAUDE.md carries the resolution as the agent's first, mandatory job.
func TestResume_UnreconcilableBranch_BecomesTheAgentsFirstJob(t *testing.T) {
	sg := &scriptedGit{}
	sg.script(argsContainAll("ls-remote", "--exit-code"), nil)
	sg.script(argsContainAll("merge-base", "--is-ancestor"), fmt.Errorf("exit status 1"))
	sg.script(notAbortMerge, fmt.Errorf("CONFLICT (content): Merge conflict in internal/bootstrap/skills.go"))

	var logBuf bytes.Buffer
	reg := prometheus.NewRegistry()
	p := resumeParams(t, "tatara/task-abc")
	p.Log = slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	p.M = metrics.New(reg)

	require.NoError(t, bootstrap.Render(p, sg.run),
		"a merge conflict on a resumed branch must never fail the pod boot")

	require.NotEmpty(t, callsEqual(sg.Calls, "merge", "--abort"),
		"a conflicting merge must be aborted so the agent's committed work and working tree are restored")
	require.Equal(t, float64(1), counterValue(t, reg, "ccw_bootstrap_reconcile_total", "conflict"))
	require.Contains(t, logBuf.String(), "bootstrap_reconcile")
	require.Contains(t, logBuf.String(), `"level":"ERROR"`)

	md := injectedGlobalClaudeMd(t, p)
	require.Contains(t, md, "Resumed task branch", "the agent must be told the branch was resumed")
	require.Contains(t, md, "hard requirement",
		"an unresolved base merge must reach the agent as a hard requirement, not a hint")
	require.Contains(t, md, "tatara/task-abc", "the directive must name the branch to reconcile")
	require.Contains(t, md, "origin/main", "the directive must name the base ref to merge")
}

// TestResume_MultiRepo_ConflictDoesNotStopRemainingRepos is the regression that
// matters most in the inversion: the old code returned from Render on the first
// conflict, so every repo after it went uncloned and the whole post-loop config
// render (CLAUDE.md, .mcp.json, settings, skills) never ran. A conflict in one
// repo must now cost that repo's merge and nothing else.
func TestResume_MultiRepo_ConflictDoesNotStopRemainingRepos(t *testing.T) {
	sg := &scriptedGit{}
	sg.script(argsContainAll("ls-remote", "--exit-code"), nil)
	sg.script(argsContainAll("merge-base", "--is-ancestor"), fmt.Errorf("exit status 1"))
	// Only repo "a" (base origin/main) conflicts; repo "b" (base origin/trunk) merges cleanly.
	sg.script(func(a []string) bool { return notAbortMerge(a) && argsContainAll("origin/main")(a) },
		fmt.Errorf("CONFLICT (content): Merge conflict in README.md"))

	var logBuf bytes.Buffer
	reg := prometheus.NewRegistry()
	p := resumeParams(t, "tatara/task-abc")
	p.RepoURL = ""
	p.RepoBranch = ""
	p.Log = slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	p.M = metrics.New(reg)
	p.ProjectClaudeMd = "project rules"
	p.Repos = []bootstrap.RepoSpec{
		{Name: "a", URL: "https://github.com/o/a", Branch: "main"},
		{Name: "b", URL: "https://github.com/o/b", Branch: "trunk"},
	}

	require.NoError(t, bootstrap.Render(p, sg.run))

	// Repo b is still cloned, still checked out, still reconciled.
	require.NotEmpty(t, callsContainingAll(sg.Calls, "clone", "https://github.com/o/b"),
		"a conflict in the primary repo must not skip the remaining clones")
	require.Len(t, callsContainingAll(sg.Calls, "checkout", "-B", "tatara/task-abc", "FETCH_HEAD"), 2,
		"both repos must have the task branch checked out")
	require.NotEmpty(t, callsContainingAll(sg.Calls, "merge", "origin/trunk"))
	require.Len(t, callsEqual(sg.Calls, "merge", "--abort"), 1,
		"only the conflicting repo's merge is aborted")
	require.Equal(t, float64(1), counterValue(t, reg, "ccw_bootstrap_reconcile_total", "conflict"))
	require.Equal(t, float64(1), counterValue(t, reg, "ccw_bootstrap_reconcile_total", "merged"))

	// The whole post-loop config render still runs.
	require.FileExists(t, filepath.Join(p.Workspace, "CLAUDE.md"))
	require.FileExists(t, filepath.Join(p.Workspace, ".mcp.json"))
	require.FileExists(t, filepath.Join(p.HomeDir, ".claude", "settings.json"))
	require.Contains(t, logBuf.String(), "bootstrap_render")

	// Both tiers are rendered, each naming its own repo directory and base ref.
	md := injectedGlobalClaudeMd(t, p)
	require.Contains(t, md, "hard requirement")
	require.Contains(t, md, filepath.Join(p.Workspace, "o", "a"),
		"the conflicting repo's on-disk directory must be named")
	require.Contains(t, md, "origin/main")
	require.Contains(t, md, filepath.Join(p.Workspace, "o", "b"),
		"the cleanly resumed repo must be reported too")
}

// TestResume_MergedResult_LogsAndCounts asserts the successful-but-stale case
// is itself observable: operators must be able to see that resumes are landing
// on stale trees at all (rules 12+13).
func TestResume_MergedResult_LogsAndCounts(t *testing.T) {
	sg := &scriptedGit{}
	sg.script(argsContainAll("ls-remote", "--exit-code"), nil)
	sg.script(argsContainAll("merge-base", "--is-ancestor"), fmt.Errorf("exit status 1"))

	var logBuf bytes.Buffer
	reg := prometheus.NewRegistry()
	p := resumeParams(t, "tatara/task-abc")
	p.Log = slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	p.M = metrics.New(reg)

	require.NoError(t, bootstrap.Render(p, sg.run))
	require.Equal(t, float64(1), counterValue(t, reg, "ccw_bootstrap_reconcile_total", "merged"))
	require.Contains(t, logBuf.String(), "bootstrap_reconcile")
	require.Contains(t, logBuf.String(), `"result":"merged"`)
	require.Contains(t, logBuf.String(), `"base_ref":"origin/main"`)

	// Tier one: a clean resume still tells the agent the branch is not fresh,
	// but states no requirement - there is nothing left to reconcile.
	md := injectedGlobalClaudeMd(t, p)
	require.Contains(t, md, "Resumed task branch")
	require.Contains(t, md, "tatara/task-abc")
	require.NotContains(t, md, "hard requirement",
		"a cleanly merged resume must not manufacture a mandatory first job")
}

// TestFreshBranch_NotReconciled asserts the fresh-task path (branch absent on
// origin, branched straight off the just-cloned base) does no reconcile work.
func TestFreshBranch_NotReconciled(t *testing.T) {
	sg := &scriptedGit{}
	sg.script(argsContainAll("ls-remote", "--exit-code"), fmt.Errorf("exit status 2"))

	require.NoError(t, bootstrap.Render(resumeParams(t, "tatara/task-new"), sg.run))
	require.Empty(t, callsContainingAll(sg.Calls, "merge-base"))
	require.Empty(t, callsContainingAll(sg.Calls, "merge", "origin/main"))
}

// TestReviewCheckoutBranch_NotReconciled asserts a read-only CHECKOUT_BRANCH
// (an MR review agent on someone else's PR head) is never merged with the base
// branch: the reviewer must see exactly the tree that is proposed, and the
// wrapper must not mutate a branch it does not own.
func TestReviewCheckoutBranch_NotReconciled(t *testing.T) {
	sg := &scriptedGit{}
	sg.script(argsContainAll("ls-remote", "--exit-code"), nil)
	sg.script(argsContainAll("merge-base", "--is-ancestor"), fmt.Errorf("exit status 1"))

	p := resumeParams(t, "")
	p.CheckoutBranch = "feature/someone-elses-pr"
	require.NoError(t, bootstrap.Render(p, sg.run))

	require.NotEmpty(t, callsContainingAll(sg.Calls, "checkout", "-B", "feature/someone-elses-pr", "FETCH_HEAD"))
	require.Empty(t, callsContainingAll(sg.Calls, "merge-base"), "a read-only review checkout must not be reconciled")
	require.Empty(t, callsContainingAll(sg.Calls, "merge", "origin/main"))
}

// TestResume_MultiRepo_ReconcilesEveryRepo asserts every repo in the workspace
// is reconciled, not just the primary: a stale secondary clone is the same
// false view of the code.
func TestResume_MultiRepo_ReconcilesEveryRepo(t *testing.T) {
	sg := &scriptedGit{}
	sg.script(argsContainAll("ls-remote", "--exit-code"), nil)
	sg.script(argsContainAll("merge-base", "--is-ancestor"), fmt.Errorf("exit status 1"))

	reg := prometheus.NewRegistry()
	p := resumeParams(t, "tatara/task-abc")
	p.RepoURL = ""
	p.RepoBranch = ""
	p.M = metrics.New(reg)
	p.Repos = []bootstrap.RepoSpec{
		{Name: "a", URL: "https://github.com/o/a", Branch: "main"},
		{Name: "b", URL: "https://github.com/o/b", Branch: "trunk"},
	}
	require.NoError(t, bootstrap.Render(p, sg.run))

	require.NotEmpty(t, callsEqual(sg.Calls, "merge-base", "--is-ancestor", "origin/main", "HEAD"))
	require.NotEmpty(t, callsEqual(sg.Calls, "merge-base", "--is-ancestor", "origin/trunk", "HEAD"),
		"each repo reconciles against its OWN base branch")
	require.Len(t, callsContainingAll(sg.Calls, "merge", "origin/main"), 1)
	require.Len(t, callsContainingAll(sg.Calls, "merge", "origin/trunk"), 1)
	require.Equal(t, float64(2), counterValue(t, reg, "ccw_bootstrap_reconcile_total", "merged"))
}

// TestResume_NoBaseBranchConfigured_ResolvesRemoteHead asserts that when the
// operator pinned no base branch, the remote's own default head is resolved
// and used, rather than skipping the reconcile.
func TestResume_NoBaseBranchConfigured_ResolvesRemoteHead(t *testing.T) {
	sg := &scriptedGit{}
	sg.script(argsContainAll("ls-remote", "--exit-code"), nil)
	sg.script(argsContainAll("merge-base", "--is-ancestor"), fmt.Errorf("exit status 1"))

	p := resumeParams(t, "tatara/task-abc")
	p.RepoBranch = ""
	require.NoError(t, bootstrap.Render(p, sg.run))

	require.NotEmpty(t, callsEqual(sg.Calls, "remote", "set-head", "origin", "--auto"))
	require.NotEmpty(t, callsEqual(sg.Calls, "merge-base", "--is-ancestor", "origin/HEAD", "HEAD"))
	require.NotEmpty(t, callsContainingAll(sg.Calls, "merge", "origin/HEAD"))
}

// TestResume_BaseBranchUnresolvable_WarnsAndCounts asserts the one case where
// the reconcile cannot even be attempted (no pinned base AND the remote head
// will not resolve) stays visible: WARN plus a counted result, never a silent
// stale resume. It does not hard-fail, because nothing here proves the tree IS
// stale - unlike a real conflict, which does.
func TestResume_BaseBranchUnresolvable_WarnsAndCounts(t *testing.T) {
	sg := &scriptedGit{}
	sg.script(argsContainAll("ls-remote", "--exit-code"), nil)
	sg.script(argsContainAll("set-head"), fmt.Errorf("exit status 128"))

	var logBuf bytes.Buffer
	reg := prometheus.NewRegistry()
	p := resumeParams(t, "tatara/task-abc")
	p.RepoBranch = ""
	p.Log = slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	p.M = metrics.New(reg)

	require.NoError(t, bootstrap.Render(p, sg.run))
	require.Empty(t, callsContainingAll(sg.Calls, "merge-base"))
	require.Equal(t, float64(1), counterValue(t, reg, "ccw_bootstrap_reconcile_total", "base_unresolved"))
	require.Contains(t, logBuf.String(), `"level":"WARN"`)
	require.Contains(t, logBuf.String(), "bootstrap_reconcile")
}

// TestResume_FetchFailure_StaysFatal pins the arm that did NOT invert. A
// conflict is now the agent's job, but a failed fetch still fails Render: a
// conflict is a known, describable state the agent can be handed, whereas a
// failed fetch means we never saw the remote at all - the resumed branch may
// not even be the branch the operator meant, and there is nothing truthful to
// tell the agent about it. Asserted explicitly so the two arms cannot drift.
func TestResume_FetchFailure_StaysFatal(t *testing.T) {
	sg := &scriptedGit{}
	sg.script(argsContainAll("ls-remote", "--exit-code"), nil)
	sg.script(func(a []string) bool { return len(a) == 2 && a[0] == "fetch" && a[1] == "origin" },
		fmt.Errorf("could not read from remote repository"))

	reg := prometheus.NewRegistry()
	p := resumeParams(t, "tatara/task-abc")
	p.M = metrics.New(reg)

	err := bootstrap.Render(p, sg.run)
	require.Error(t, err, "a reconcile whose fetch failed must still fail bootstrap")
	require.Contains(t, err.Error(), "reconcile")
	require.Equal(t, float64(1), counterValue(t, reg, "ccw_bootstrap_reconcile_total", "fetch_fail"))
	require.NoFileExists(t, filepath.Join(p.HomeDir, ".claude", "CLAUDE.md"),
		"Render aborts on a fetch failure, so no agent directive is rendered at all")
}
