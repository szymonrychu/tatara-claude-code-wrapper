package bootstrap_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
)

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

// TestResume_UnreconcilableBranch_FailsLoud asserts sub-problem B: a resumed
// branch that cannot be reconciled (genuine merge conflict) aborts the merge,
// increments the reconcile counter with result=conflict, logs at ERROR, and
// fails Render - it never falls through to a silent stale resume.
func TestResume_UnreconcilableBranch_FailsLoud(t *testing.T) {
	sg := &scriptedGit{}
	sg.script(argsContainAll("ls-remote", "--exit-code"), nil)
	sg.script(argsContainAll("merge-base", "--is-ancestor"), fmt.Errorf("exit status 1"))
	sg.script(notAbortMerge, fmt.Errorf("CONFLICT (content): Merge conflict in internal/bootstrap/skills.go"))

	var logBuf bytes.Buffer
	reg := prometheus.NewRegistry()
	p := resumeParams(t, "tatara/task-abc")
	p.Log = slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	p.M = metrics.New(reg)

	err := bootstrap.Render(p, sg.run)
	require.Error(t, err, "an unreconcilable resumed branch must fail bootstrap, not resume silently")
	require.Contains(t, err.Error(), "reconcile")

	require.NotEmpty(t, callsEqual(sg.Calls, "merge", "--abort"),
		"a conflicting merge must be aborted so the agent's committed work and working tree are restored")
	require.Equal(t, float64(1), counterValue(t, reg, "ccw_bootstrap_reconcile_total", "conflict"))
	require.Contains(t, logBuf.String(), "bootstrap_reconcile")
	require.Contains(t, logBuf.String(), `"level":"ERROR"`)
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

// TestResume_FetchFailure_FailsLoud asserts a reconcile whose fetch fails is a
// hard error: without a fresh view of the remote we cannot claim the tree is
// current, which is the exact silent failure #131 reported.
func TestResume_FetchFailure_FailsLoud(t *testing.T) {
	sg := &scriptedGit{}
	sg.script(argsContainAll("ls-remote", "--exit-code"), nil)
	sg.script(func(a []string) bool { return len(a) == 2 && a[0] == "fetch" && a[1] == "origin" },
		fmt.Errorf("could not read from remote repository"))

	reg := prometheus.NewRegistry()
	p := resumeParams(t, "tatara/task-abc")
	p.M = metrics.New(reg)

	err := bootstrap.Render(p, sg.run)
	require.Error(t, err)
	require.Contains(t, err.Error(), "reconcile")
	require.Equal(t, float64(1), counterValue(t, reg, "ccw_bootstrap_reconcile_total", "fetch_fail"))
}
