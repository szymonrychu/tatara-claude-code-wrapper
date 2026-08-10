package bootstrap_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
)

// scrubbedGitEnv is the environment every git call in this file runs under. It
// drops EVERY inherited GIT_* variable before adding the ones we choose.
//
// That is not defensive tidiness, it is required for correctness. `go test` can
// run from inside a git hook - this repo's own pre-push hook runs `make test` -
// and git exports GIT_DIR and GIT_WORK_TREE to its hooks. Inheriting them points
// every command here at the DEVELOPER'S repository instead of the temp one, so
// `git init --bare <tmpdir>` re-initialises the real checkout as bare and
// corrupts its config. This is not hypothetical: it happened once, and the
// symptom (`core.bare and core.worktree do not make sense`) looks like a broken
// checkout rather than a broken test.
func scrubbedGitEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}

// realGit is a GitRunner backed by the actual git binary, with HOME and the
// system config redirected so a developer's own git configuration cannot change
// the outcome. The workspace contract is about what git really does to a real
// repository - a scripted fake would happily "prove" behaviour git does not
// have.
func realGit(t *testing.T) bootstrap.GitRunner {
	t.Helper()
	home := t.TempDir()
	return func(dir string, args ...string) error {
		cmd := exec.Command("git", args...) //nolint:gosec // test harness: git is a fixed binary and the args come from this file
		cmd.Dir = dir
		cmd.Env = scrubbedGitEnv(
			"HOME="+home,
			"GIT_CONFIG_GLOBAL="+filepath.Join(home, "gitconfig"),
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=tatara", "GIT_AUTHOR_EMAIL=tatara@example.invalid",
			"GIT_COMMITTER_NAME=tatara", "GIT_COMMITTER_EMAIL=tatara@example.invalid",
			"GIT_TERMINAL_PROMPT=0",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
		}
		return nil
	}
}

// gitOut runs git for its stdout. Only tests may do this: the production
// GitRunner drops output deliberately (git stderr can carry credential-helper
// expansions and tokenised remote URLs).
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // test harness: git is a fixed binary and the args come from this file
	cmd.Dir = dir
	cmd.Env = scrubbedGitEnv("GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	require.NoErrorf(t, err, "git %s", strings.Join(args, " "))
	return strings.TrimSpace(string(out))
}

// writeFile writes name under dir, creating parents.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
}

// commitFile writes a file and commits it, returning the new HEAD sha.
func commitFile(t *testing.T, git bootstrap.GitRunner, dir, name, content, msg string) string {
	t.Helper()
	writeFile(t, dir, name, content)
	require.NoError(t, git(dir, "add", "-A"))
	require.NoError(t, git(dir, "commit", "--no-verify", "-m", msg))
	return gitOut(t, dir, "rev-parse", "HEAD")
}

// initWorkRepo creates a non-bare repo on branch main with one commit.
func initWorkRepo(t *testing.T, git bootstrap.GitRunner) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, git(dir, "init", "-b", "main", dir))
	commitFile(t, git, dir, "README.md", "hello\n", "initial")
	return dir
}

// initBareRemote creates a bare repo with a main branch carrying one commit,
// and returns its path. The path doubles as the clone URL: namespacePath maps
// any multi-segment path to an on-disk namespace, and a local-path clone gives
// the tests a real remote with no network.
func initBareRemote(t *testing.T, git bootstrap.GitRunner) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "owner", "repo.git")
	require.NoError(t, os.MkdirAll(bare, 0o755))
	require.NoError(t, git(bare, "init", "--bare", "-b", "main", bare))
	seed := initWorkRepo(t, git)
	require.NoError(t, git(seed, "remote", "add", "origin", bare))
	require.NoError(t, git(seed, "push", "-u", "origin", "main"))
	return bare
}

// workspaceParams is a Params for a single-repo workspace backed by a real
// local remote. FullClone avoids the shallow-history noise a local clone would
// silently ignore anyway.
func workspaceParams(t *testing.T, ws, remote, taskBranch string) bootstrap.Params {
	t.Helper()
	return bootstrap.Params{
		HomeDir:        t.TempDir(),
		Workspace:      ws,
		BaseMCP:        []byte(`{"mcpServers":{}}`),
		RepoURL:        remote,
		RepoBranch:     "main",
		TaskBranch:     taskBranch,
		FullClone:      true,
		HookCommand:    "/usr/local/bin/cc-stop-hook",
		PermissionMode: "bypassPermissions",
		TaskName:       "task-alpha",
		ProjectName:    "proj",
	}
}

// TestResumedWorkspace_LocalBranchWithoutRemote_IsNotFatal is headline case
// one. The pod committed to the task branch and died before anything reached
// origin - exactly the OOMKill that motivated the per-Task volume. On the next
// boot the branch exists locally and NOT on origin, and the old bootstrap ran
// `git checkout -b <taskBranch>`, which git refuses when the branch already
// exists. With a persistent workspace that refusal is permanent: the pod
// crash-loops until the residency cap and no turn ever runs.
func TestResumedWorkspace_LocalBranchWithoutRemote_IsNotFatal(t *testing.T) {
	git := realGit(t)
	remote := initBareRemote(t, git)
	ws := t.TempDir()
	p := workspaceParams(t, ws, remote, "tatara/task-alpha")

	require.NoError(t, bootstrap.Render(p, git))
	dest := bootstrap.RepoDir(ws, remote)
	localSHA := commitFile(t, git, dest, "work.md", "committed, never pushed\n", "agent work")

	// Second boot on the SAME workspace volume: the task branch exists locally,
	// origin has never heard of it.
	require.NoError(t, bootstrap.Render(p, git),
		"a local task branch with no remote counterpart must not fail the boot")

	require.Equal(t, "tatara/task-alpha", gitOut(t, dest, "rev-parse", "--abbrev-ref", "HEAD"))
	require.Equal(t, localSHA, gitOut(t, dest, "rev-parse", "HEAD"),
		"the unpushed commit must still be the tip of the task branch")
	require.Equal(t, localSHA, gitOut(t, remote, "rev-parse", "refs/heads/tatara/task-alpha"),
		"a branch that exists only on this volume must be published, or one lost volume loses it")
}

// TestResumedWorkspace_DivergedBranch_KeepsLocalCommits is headline case two.
// Local and remote both carry the task branch and have diverged. The old
// bootstrap ran `git checkout -B <branch> FETCH_HEAD`, which hard-resets the
// local branch onto the remote tip and destroys precisely the unpushed commits
// the volume exists to preserve. The contract is additive: merge, never reset,
// and never rebase - force-push is denied, so a rewritten branch is stranded.
func TestResumedWorkspace_DivergedBranch_KeepsLocalCommits(t *testing.T) {
	git := realGit(t)
	remote := initBareRemote(t, git)
	ws := t.TempDir()
	p := workspaceParams(t, ws, remote, "tatara/task-alpha")

	require.NoError(t, bootstrap.Render(p, git))
	dest := bootstrap.RepoDir(ws, remote)

	// A previous turn published one commit on the task branch.
	pushedSHA := commitFile(t, git, dest, "shared.md", "turn one\n", "turn one")
	require.NoError(t, git(dest, "push", "-u", "origin", "tatara/task-alpha"))

	// Someone else advanced the remote task branch (a mid-turn safety-net push
	// from a pod that then died, or the operator).
	other := t.TempDir()
	require.NoError(t, git(filepath.Dir(other), "clone", remote, other))
	require.NoError(t, git(other, "checkout", "tatara/task-alpha"))
	remoteSHA := commitFile(t, git, other, "remote-only.md", "from origin\n", "remote work")
	require.NoError(t, git(other, "push", "origin", "tatara/task-alpha"))

	// ...while this volume made its own commit that never reached origin.
	localSHA := commitFile(t, git, dest, "local-only.md", "never pushed\n", "local work")

	require.NoError(t, bootstrap.Render(p, git),
		"a diverged task branch must not fail the boot")

	require.Equal(t, "tatara/task-alpha", gitOut(t, dest, "rev-parse", "--abbrev-ref", "HEAD"))
	require.NoError(t, git(dest, "merge-base", "--is-ancestor", localSHA, "HEAD"),
		"the unpushed local commit must still be reachable from the task branch")
	require.NoError(t, git(dest, "merge-base", "--is-ancestor", remoteSHA, "HEAD"),
		"the remote commit must be merged in, not ignored")
	require.NoError(t, git(dest, "merge-base", "--is-ancestor", pushedSHA, "HEAD"))
	require.FileExists(t, filepath.Join(dest, "local-only.md"))
	require.FileExists(t, filepath.Join(dest, "remote-only.md"))
}

// requirePathExists / requirePathGone accept either a file or a directory:
// git's in-progress markers are both (MERGE_HEAD is a file, rebase-merge is a
// directory), and require.FileExists rejects directories.
func requirePathExists(t *testing.T, path, msg string) {
	t.Helper()
	_, err := os.Stat(path)
	require.NoErrorf(t, err, "%s: %s", msg, path)
}

func requirePathGone(t *testing.T, path, msg string) {
	t.Helper()
	_, err := os.Stat(path)
	require.Truef(t, os.IsNotExist(err), "%s: %s still exists", msg, path)
}

// tryGit runs git and tolerates a non-zero exit, for the deliberately
// conflicting commands that put a repo into a mid-operation state.
func tryGit(git bootstrap.GitRunner, dir string, args ...string) {
	_ = git(dir, args...)
}

// countedRecovery reads ccw_bootstrap_workspace_recovered_total for one kind.
func countedRecovery(t *testing.T, reg *prometheus.Registry, name, label, value string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	var sum float64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, mt := range mf.GetMetric() {
			for _, lp := range mt.GetLabel() {
				if lp.GetName() == label && lp.GetValue() == value {
					sum += mt.GetCounter().GetValue()
				}
			}
		}
	}
	return sum
}

// TestPrepareWorkspace_HalfCloneIsWiped covers Phase A. A pod killed mid-clone
// leaves a .git that git will not open; every later phase would fail against
// it, and the .git stat that decides whether to clone would see it and skip the
// clone forever. The wipe is what lets the caller fall through to a fresh one.
func TestPrepareWorkspace_HalfCloneIsWiped(t *testing.T) {
	git := realGit(t)
	dest := filepath.Join(t.TempDir(), "owner", "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(dest, ".git", "objects"), 0o755))
	writeFile(t, dest, ".git/HEAD", "not a ref at all\n")
	writeFile(t, dest, "leftover.md", "half a clone\n")

	reg := prometheus.NewRegistry()
	prep, err := bootstrap.PrepareWorkspace(dest, git, nil, metrics.New(reg))
	require.NoError(t, err)
	require.True(t, prep.Wiped)
	require.NoDirExists(t, dest, "an unusable checkout must be removed so a fresh clone can run")
	require.Empty(t, prep.Notes, "a half-clone is platform noise, not something the agent can act on")
	require.Equal(t, float64(1),
		countedRecovery(t, reg, "ccw_bootstrap_workspace_recovered_total", "kind", "invalid git"))
}

// TestPrepareWorkspace_HealthyRepoUntouched is the other half of Phase A: be
// conservative. A repo that is merely idle must survive untouched - a false
// positive here destroys the volume's whole reason to exist.
func TestPrepareWorkspace_HealthyRepoUntouched(t *testing.T) {
	git := realGit(t)
	dest := initWorkRepo(t, git)
	before := gitOut(t, dest, "rev-parse", "HEAD")

	prep, err := bootstrap.PrepareWorkspace(dest, git, nil, nil)
	require.NoError(t, err)
	require.False(t, prep.Wiped)
	require.Empty(t, prep.Notes)
	require.Equal(t, before, gitOut(t, dest, "rev-parse", "HEAD"))
}

// TestPrepareWorkspace_ClearsStaleLocks covers Phase B. Nothing in the wrapper
// removes these today, and each one makes its git operation fail permanently:
// the process that held it died with the pod, so it is stale by construction on
// a freshly booted pod. Removal is unconditional for that reason - an mtime
// heuristic is a trap on a volume whose clock outlived the pod.
func TestPrepareWorkspace_ClearsStaleLocks(t *testing.T) {
	locks := []struct {
		rel   string
		label string
	}{
		{".git/index.lock", "index.lock"},
		{".git/shallow.lock", "shallow.lock"},
		{".git/HEAD.lock", "HEAD.lock"},
		{".git/config.lock", "config.lock"},
		{".git/packed-refs.lock", "packed-refs.lock"},
		{".git/refs/heads/main.lock", "refs"},
		{".git/refs/heads/tatara/task-alpha.lock", "refs"},
		{".git/refs/remotes/origin/main.lock", "refs"},
	}
	for _, lock := range locks {
		t.Run(lock.rel, func(t *testing.T) {
			git := realGit(t)
			dest := initWorkRepo(t, git)
			writeFile(t, dest, lock.rel, "")

			reg := prometheus.NewRegistry()
			prep, err := bootstrap.PrepareWorkspace(dest, git, nil, metrics.New(reg))
			require.NoError(t, err)
			require.False(t, prep.Wiped)
			require.NoFileExists(t, filepath.Join(dest, lock.rel))
			require.Empty(t, prep.Notes, "a stale lock is platform noise and must not reach the agent")
			require.Equal(t, float64(1),
				countedRecovery(t, reg, "ccw_bootstrap_lock_cleared_total", "lock", lock.label))
		})
	}
}

// TestPrepareWorkspace_ClearsLocksWithoutTouchingRefs asserts the ref sweep
// removes the lock and nothing else - deleting the ref itself would lose the
// branch the lock was protecting.
func TestPrepareWorkspace_ClearsLocksWithoutTouchingRefs(t *testing.T) {
	git := realGit(t)
	dest := initWorkRepo(t, git)
	head := gitOut(t, dest, "rev-parse", "HEAD")
	writeFile(t, dest, ".git/refs/heads/main.lock", "")

	_, err := bootstrap.PrepareWorkspace(dest, git, nil, nil)
	require.NoError(t, err)
	require.NoFileExists(t, filepath.Join(dest, ".git", "refs", "heads", "main.lock"))
	require.Equal(t, head, gitOut(t, dest, "rev-parse", "refs/heads/main"),
		"the ref the lock guarded must survive the sweep")
}

// TestPrepareWorkspace_AbortsInProgressOperations covers Phase C over real
// mid-operation repositories. Leaving any of these in place would hand the turn
// finaliser a conflicted index, and `git add -A` plus `git commit --no-verify`
// would publish the conflict markers to the task branch.
func TestPrepareWorkspace_AbortsInProgressOperations(t *testing.T) {
	// conflictRepo builds main and b1 with contradictory edits to f.txt, plus a
	// third commit, so every flavour of mid-operation state below can be made
	// for real rather than by planting a marker file.
	conflictRepo := func(t *testing.T, git bootstrap.GitRunner) (dir, otherBranchSHA, middleSHA string) {
		t.Helper()
		dir = t.TempDir()
		require.NoError(t, git(dir, "init", "-b", "main", dir))
		commitFile(t, git, dir, "f.txt", "one\n", "c1")
		middleSHA = commitFile(t, git, dir, "f.txt", "two\n", "c2")
		commitFile(t, git, dir, "f.txt", "three\n", "c3")
		require.NoError(t, git(dir, "checkout", "-b", "b1", middleSHA))
		otherBranchSHA = commitFile(t, git, dir, "f.txt", "branch side\n", "b1")
		require.NoError(t, git(dir, "checkout", "main"))
		return dir, otherBranchSHA, middleSHA
	}

	cases := []struct {
		name   string
		marker string
		kind   string
		start  func(t *testing.T, git bootstrap.GitRunner, dir, branchSHA, middleSHA string)
	}{
		{"merge", "MERGE_HEAD", "aborted merge", func(_ *testing.T, git bootstrap.GitRunner, dir, sha, _ string) {
			tryGit(git, dir, "merge", sha)
		}},
		{"rebase merge backend", "rebase-merge", "aborted rebase", func(_ *testing.T, git bootstrap.GitRunner, dir, sha, _ string) {
			tryGit(git, dir, "rebase", sha)
		}},
		{"rebase apply backend", "rebase-apply", "aborted rebase", func(_ *testing.T, git bootstrap.GitRunner, dir, sha, _ string) {
			tryGit(git, dir, "rebase", "--apply", sha)
		}},
		{"cherry pick", "CHERRY_PICK_HEAD", "aborted cherry pick", func(_ *testing.T, git bootstrap.GitRunner, dir, sha, _ string) {
			tryGit(git, dir, "cherry-pick", sha)
		}},
		{"revert", "REVERT_HEAD", "aborted revert", func(_ *testing.T, git bootstrap.GitRunner, dir, _, middle string) {
			tryGit(git, dir, "revert", "--no-edit", middle)
		}},
		{"bisect", "BISECT_LOG", "aborted bisect", func(_ *testing.T, git bootstrap.GitRunner, dir, _, _ string) {
			tryGit(git, dir, "bisect", "start")
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			git := realGit(t)
			dir, branchSHA, middleSHA := conflictRepo(t, git)
			tipBefore := gitOut(t, dir, "rev-parse", "refs/heads/main")
			tc.start(t, git, dir, branchSHA, middleSHA)
			requirePathExists(t, filepath.Join(dir, ".git", tc.marker),
				"the test must actually reach the mid-operation state it claims to")

			reg := prometheus.NewRegistry()
			prep, err := bootstrap.PrepareWorkspace(dir, git, nil, metrics.New(reg))
			require.NoError(t, err)
			require.False(t, prep.Wiped)
			requirePathGone(t, filepath.Join(dir, ".git", tc.marker),
				"the interrupted operation must be aborted, not carried into the turn")
			require.Equal(t, tipBefore, gitOut(t, dir, "rev-parse", "refs/heads/main"),
				"an abort restores HEAD exactly and must never move a branch")
			require.NotEmpty(t, prep.Notes,
				"an aborted operation changes what the agent will find and must be surfaced")
			require.Contains(t, prep.Notes[0], "aborted")
			require.Equal(t, float64(1),
				countedRecovery(t, reg, "ccw_bootstrap_workspace_recovered_total", "kind", tc.kind))
		})
	}
}

// TestPrepareWorkspace_DetachedHeadRecorded covers the Phase D detached case.
// It is counted, not surfaced: Phase E's checkout reattaches HEAD, so by the
// time the agent reads anything there is nothing left to act on.
func TestPrepareWorkspace_DetachedHeadRecorded(t *testing.T) {
	git := realGit(t)
	dest := initWorkRepo(t, git)
	commitFile(t, git, dest, "second.md", "second\n", "c2")
	require.NoError(t, git(dest, "checkout", "--detach", "HEAD~1"))

	reg := prometheus.NewRegistry()
	prep, err := bootstrap.PrepareWorkspace(dest, git, nil, metrics.New(reg))
	require.NoError(t, err)
	require.Empty(t, prep.Notes, "the agent cannot act on a detachment the checkout already fixed")
	require.Equal(t, float64(1),
		countedRecovery(t, reg, "ccw_bootstrap_workspace_recovered_total", "kind", "detached head"))
}

// TestPrepareWorkspace_DirtyTreeIsCommittedWithoutOversizedBlob covers the
// heart of Phase D. Uncommitted work is COMMITTED, never stashed (a stash is
// invisible to the agent and to the turn finaliser's `git add -A`, so it is a
// slower way of losing the work) and never discarded. The oversized-blob guard
// runs first, so a build artifact the dead session left behind is not swept
// onto the branch for every later turn to inherit.
func TestPrepareWorkspace_DirtyTreeIsCommittedWithoutOversizedBlob(t *testing.T) {
	orig := bootstrap.MaxStagedBlobBytes
	bootstrap.MaxStagedBlobBytes = 1024
	t.Cleanup(func() { bootstrap.MaxStagedBlobBytes = orig })

	git := realGit(t)
	dest := initWorkRepo(t, git)
	before := gitOut(t, dest, "rev-parse", "HEAD")

	writeFile(t, dest, "README.md", "edited but never committed\n") // tracked, modified
	writeFile(t, dest, "new-source.go", "package main\n")           // untracked source
	require.NoError(t, os.WriteFile(filepath.Join(dest, "wrapper"), make([]byte, 4096), 0o755))

	reg := prometheus.NewRegistry()
	prep, err := bootstrap.PrepareWorkspace(dest, git, nil, metrics.New(reg))
	require.NoError(t, err)
	require.False(t, prep.Wiped)

	after := gitOut(t, dest, "rev-parse", "HEAD")
	require.NotEqual(t, before, after, "uncommitted work must be rescued into a commit")
	require.Contains(t, gitOut(t, dest, "log", "-1", "--pretty=%s"),
		"rescue uncommitted work from an interrupted session")

	tracked := gitOut(t, dest, "show", "--name-only", "--pretty=format:", "HEAD")
	require.Contains(t, tracked, "README.md")
	require.Contains(t, tracked, "new-source.go")
	require.NotContains(t, tracked, "wrapper",
		"a build artifact must never be swept onto the branch by the rescue commit")
	require.FileExists(t, filepath.Join(dest, "wrapper"), "the artifact stays on disk, it is only unstaged")

	require.NotEmpty(t, prep.Notes)
	require.Contains(t, prep.Notes[0], after[:12], "the note must name the rescue commit so the agent can find it")
	require.Equal(t, float64(1),
		countedRecovery(t, reg, "ccw_bootstrap_workspace_recovered_total", "kind", "rescue commit"))
	require.Equal(t, float64(1), counterValue(t, reg, "ccw_commit_oversized_blob_skipped_total", ""))
}

// TestPrepareWorkspace_UntrackedOnlyLeftAlone: gitignored and untracked build
// output is the normal steady state of a persistent workspace, not damage.
// Committing on every boot because `node_modules` exists would be noise.
func TestPrepareWorkspace_UntrackedOnlyLeftAlone(t *testing.T) {
	git := realGit(t)
	dest := initWorkRepo(t, git)
	before := gitOut(t, dest, "rev-parse", "HEAD")
	writeFile(t, dest, "build/output.bin", "artifact\n")

	prep, err := bootstrap.PrepareWorkspace(dest, git, nil, nil)
	require.NoError(t, err)
	require.Equal(t, before, gitOut(t, dest, "rev-parse", "HEAD"),
		"an untracked-only tree is the steady state and must not produce a commit")
	require.Empty(t, prep.Notes)
	require.FileExists(t, filepath.Join(dest, "build", "output.bin"))
}

// TestPrepareWorkspace_DetachedRescueStaysReachable: a rescue commit made while
// HEAD was detached would be reachable only through the reflog once Phase E
// checks the task branch out over it. A named branch is additive and costs
// nothing; losing the commit is the one outcome this contract exists to
// prevent.
func TestPrepareWorkspace_DetachedRescueStaysReachable(t *testing.T) {
	git := realGit(t)
	dest := initWorkRepo(t, git)
	commitFile(t, git, dest, "second.md", "second\n", "c2")
	require.NoError(t, git(dest, "checkout", "--detach", "HEAD~1"))
	writeFile(t, dest, "README.md", "work done on a detached HEAD\n")

	prep, err := bootstrap.PrepareWorkspace(dest, git, nil, nil)
	require.NoError(t, err)
	rescued := gitOut(t, dest, "rev-parse", "HEAD")

	require.NoError(t, git(dest, "checkout", "main"))
	require.NoError(t, git(dest, "merge-base", "--is-ancestor", rescued, "tatara-bootstrap-rescue"),
		"a rescue commit made on a detached HEAD must stay reachable by name")
	require.NotEmpty(t, prep.Notes)
	require.Contains(t, prep.Notes[0], "tatara-bootstrap-rescue")
}

// TestWorkspaceIdentity_SameTaskResumes: the ordinary persistent-volume boot.
// The workspace belongs to this Task, so nothing is wiped and the checkout the
// previous session left behind is the one the agent gets.
func TestWorkspaceIdentity_SameTaskResumes(t *testing.T) {
	git := realGit(t)
	remote := initBareRemote(t, git)
	ws := t.TempDir()
	p := workspaceParams(t, ws, remote, "tatara/task-alpha")

	require.NoError(t, bootstrap.Render(p, git))
	require.FileExists(t, filepath.Join(ws, ".tatara-workspace.json"))
	dest := bootstrap.RepoDir(ws, remote)
	sha := commitFile(t, git, dest, "work.md", "session one\n", "session one")
	writeFile(t, ws, "scratch.txt", "notes the agent kept\n")

	require.NoError(t, bootstrap.Render(p, git))
	require.Equal(t, sha, gitOut(t, dest, "rev-parse", "HEAD"),
		"the same Task must resume its own tree, not re-clone over it")
	require.FileExists(t, filepath.Join(ws, "scratch.txt"))
}

// TestWorkspaceIdentity_DifferentTaskWipes: a per-Task volume cannot
// legitimately hold another Task's tree, so a rebound volume is wiped and
// treated as a first boot. Guessing which parts are reusable is strictly worse
// than re-cloning: a wrong guess hands the agent another Task's code as though
// it were its own.
func TestWorkspaceIdentity_DifferentTaskWipes(t *testing.T) {
	git := realGit(t)
	remote := initBareRemote(t, git)
	ws := t.TempDir()

	first := workspaceParams(t, ws, remote, "tatara/task-alpha")
	require.NoError(t, bootstrap.Render(first, git))
	dest := bootstrap.RepoDir(ws, remote)
	commitFile(t, git, dest, "work.md", "another task's work\n", "not ours")
	writeFile(t, ws, "scratch.txt", "another task's scratch\n")

	second := workspaceParams(t, ws, remote, "tatara/task-beta")
	second.TaskName = "task-beta"
	reg := prometheus.NewRegistry()
	second.M = metrics.New(reg)
	require.NoError(t, bootstrap.Render(second, git))

	require.NoFileExists(t, filepath.Join(ws, "scratch.txt"),
		"a volume rebound to another Task must be wiped, not merged into")
	require.NoFileExists(t, filepath.Join(dest, "work.md"))
	require.DirExists(t, dest, "the wipe is followed by a normal fresh clone")
	require.Equal(t, "tatara/task-beta", gitOut(t, dest, "rev-parse", "--abbrev-ref", "HEAD"))
	require.Equal(t, float64(1),
		countedRecovery(t, reg, "ccw_bootstrap_workspace_recovered_total", "kind", "wrong task"))

	md := injectedGlobalClaudeMd(t, second)
	require.NotContains(t, md, "Resumed task branch",
		"a wiped workspace is indistinguishable from a first boot and must say nothing")
}

// TestWorkspaceIdentity_ChangedRepoSetKeepsTree: a Project gaining or losing a
// repo is not a Task mismatch. The new repo is cloned normally and the removed
// one is LEFT on disk - it may hold uncommitted work, and deciding to delete it
// is not bootstrap's call - but the agent is told, or it finds a directory the
// Project does not mention and cannot tell why.
func TestWorkspaceIdentity_ChangedRepoSetKeepsTree(t *testing.T) {
	git := realGit(t)
	keep := initBareRemote(t, git)
	drop := initBareRemote(t, git)
	add := initBareRemote(t, git)
	ws := t.TempDir()

	p := workspaceParams(t, ws, "", "tatara/task-alpha")
	p.RepoURL = ""
	p.RepoBranch = ""
	p.Repos = []bootstrap.RepoSpec{
		{Name: "keep", URL: keep, Branch: "main"},
		{Name: "drop", URL: drop, Branch: "main"},
	}
	require.NoError(t, bootstrap.Render(p, git))
	dropDest := bootstrap.RepoDir(ws, drop)
	dropSHA := commitFile(t, git, dropDest, "unpushed.md", "may still matter\n", "unpushed work")

	p.Repos = []bootstrap.RepoSpec{
		{Name: "keep", URL: keep, Branch: "main"},
		{Name: "add", URL: add, Branch: "main"},
	}
	reg := prometheus.NewRegistry()
	p.M = metrics.New(reg)
	require.NoError(t, bootstrap.Render(p, git))

	require.DirExists(t, bootstrap.RepoDir(ws, add), "a newly added repo must be cloned")
	require.DirExists(t, dropDest, "a removed repo is left alone, not deleted")
	require.Equal(t, dropSHA, gitOut(t, dropDest, "rev-parse", "HEAD"),
		"a removed repo may hold uncommitted work; deleting it is not bootstrap's call")
	require.Equal(t, float64(1),
		countedRecovery(t, reg, "ccw_bootstrap_workspace_recovered_total", "kind", "repo set changed"))

	md := injectedGlobalClaudeMd(t, p)
	require.Contains(t, md, "Workspace recovered from an interrupted session")
	require.Contains(t, md, add, "the agent must be told which repo is new")
	require.Contains(t, md, drop, "the agent must be told which stale checkout to distrust")
}

// TestResumeDirective_LocalRemoteMergeBeforeBaseMerge is the ordering contract.
// Both merges conflicted and both are the agent's first job, but the base merge
// is only meaningful once the branch actually contains everything BOTH sides of
// the task branch committed: merging the base into a branch still missing half
// its own work reconciles nothing. The middle tier must also appear, carrying
// the rescue commit that the same boot made.
func TestResumeDirective_LocalRemoteMergeBeforeBaseMerge(t *testing.T) {
	git := realGit(t)
	remote := initBareRemote(t, git)
	ws := t.TempDir()
	p := workspaceParams(t, ws, remote, "tatara/task-alpha")

	require.NoError(t, bootstrap.Render(p, git))
	dest := bootstrap.RepoDir(ws, remote)
	commitFile(t, git, dest, "shared.md", "shared base\n", "shared base")
	require.NoError(t, git(dest, "push", "-u", "origin", "tatara/task-alpha"))

	// The remote task branch and the base branch both move, each editing the
	// same line the local branch is about to edit differently.
	other := t.TempDir()
	require.NoError(t, git(filepath.Dir(other), "clone", remote, other))
	require.NoError(t, git(other, "checkout", "tatara/task-alpha"))
	commitFile(t, git, other, "shared.md", "remote side\n", "remote side")
	require.NoError(t, git(other, "push", "origin", "tatara/task-alpha"))
	require.NoError(t, git(other, "checkout", "main"))
	commitFile(t, git, other, "shared.md", "base side\n", "base side")
	require.NoError(t, git(other, "push", "origin", "main"))

	commitFile(t, git, dest, "shared.md", "local side\n", "local side")
	// ...and the session was cut off with a TRACKED file still uncommitted, on a
	// file that takes no part in either conflict. Tracked matters: an
	// untracked-only tree is the steady state of a persistent workspace and is
	// deliberately left alone.
	writeFile(t, dest, "README.md", "half-written notes\n")

	reg := prometheus.NewRegistry()
	p.M = metrics.New(reg)
	require.NoError(t, bootstrap.Render(p, git),
		"two conflicting merges are two jobs for the agent, never a boot failure")

	require.Equal(t, float64(1), counterValue(t, reg, "ccw_bootstrap_reconcile_total", "local_remote_conflict"))
	require.Equal(t, float64(1), counterValue(t, reg, "ccw_bootstrap_reconcile_total", "conflict"))
	require.Empty(t, gitOut(t, dest, "status", "--porcelain"),
		"both merges must be aborted, so the turn finaliser cannot publish conflict markers")

	md := injectedGlobalClaudeMd(t, p)
	require.Contains(t, md, "Resumed task branch")
	require.Contains(t, md, "Workspace recovered from an interrupted session",
		"the rescue commit is the middle tier and must be reported")
	require.Contains(t, md, "rescue uncommitted work from an interrupted session")
	require.Contains(t, md, "hard requirement")

	localInstruction := strings.Index(md, "merge `origin/tatara/task-alpha`")
	baseInstruction := strings.Index(md, "merge `origin/main`")
	require.NotEqual(t, -1, localInstruction, "the local-versus-remote merge must be stated")
	require.NotEqual(t, -1, baseInstruction, "the base merge must be stated")
	require.Less(t, localInstruction, baseInstruction,
		"the branch's own remote must be reconciled before the base branch, or the base merge lands on a branch missing half its work")

	recovered := strings.Index(md, "Workspace recovered from an interrupted session")
	required := strings.Index(md, "Hard requirement")
	require.Less(t, recovered, required, "the informational middle tier precedes the mandatory tier")
}

// brokenRebaseState leaves dir in the shape a pod killed mid-rebase leaves it:
// a truncated .git/rebase-merge that git refuses to abort ("could not read
// head-name"), over a dirty working tree. It is the real failure, not a mock -
// `git rebase --abort` genuinely exits non-zero against it and genuinely leaves
// the tree dirty.
func brokenRebaseState(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git", "rebase-merge"), 0o755))
	writeFile(t, dir, "README.md", "<<<<<<< HEAD\nhalf-applied work\n")
	writeFile(t, dir, "extra.md", "and something new\n")
}

// TestPrepareWorkspace_FailedAbortRescuesRatherThanRecloning is the correction
// to the first cut of this contract, which re-cloned when an abort failed over
// a dirty tree. Re-cloning destroys uncommitted work, which is the exact
// outcome the whole contract exists to prevent - a failed abort is when the
// previous session's state is LEAST understood, not a licence to delete it.
//
// The tree is preserved on a dedicated rescue branch. Conflict markers may well
// be committed there, and that is fine precisely because it is not the task
// branch: PR #159's rule is that markers must never reach the branch the turn
// finaliser and the mid-turn safety net PUSH, and a local rescue branch never
// is one.
func TestPrepareWorkspace_FailedAbortRescuesRatherThanRecloning(t *testing.T) {
	git := realGit(t)
	dest := initWorkRepo(t, git)
	require.NoError(t, git(dest, "checkout", "-b", "tatara/task-alpha"))
	taskTip := commitFile(t, git, dest, "work.md", "committed work\n", "real work")
	brokenRebaseState(t, dest)

	reg := prometheus.NewRegistry()
	prep, err := bootstrap.PrepareWorkspace(dest, git, nil, metrics.New(reg))
	require.NoError(t, err)
	require.False(t, prep.Wiped, "a tree we cannot prove is disposable must never be re-cloned")
	require.DirExists(t, dest)

	require.Equal(t, taskTip, gitOut(t, dest, "rev-parse", "refs/heads/tatara/task-alpha"),
		"the task branch must not advance: it is pushed, and a rescue may carry conflict markers")
	require.Equal(t, "tatara/task-alpha", gitOut(t, dest, "rev-parse", "--abbrev-ref", "HEAD"),
		"the rescue must hand the checkout back on the branch it found it")
	require.Empty(t, gitOut(t, dest, "status", "--porcelain"),
		"everything in the tree was preserved, so nothing is left uncommitted")

	rescueTip := gitOut(t, dest, "rev-parse", "refs/heads/tatara-bootstrap-rescue-aborted")
	require.NoError(t, git(dest, "merge-base", "--is-ancestor", taskTip, "tatara-bootstrap-rescue-aborted"),
		"the commits that existed before the rescue must still be reachable from it")
	require.Contains(t, gitOut(t, dest, "show", "--name-only", "--pretty=format:", rescueTip), "extra.md",
		"the uncommitted tree is what the rescue branch exists to hold")

	require.NotEmpty(t, prep.Required,
		"a session that could not be cleanly aborted is exactly when the agent must look, so this is mandatory, not a footnote")
	require.Contains(t, prep.Required[0], "tatara-bootstrap-rescue-aborted")
	require.Contains(t, prep.Required[0], rescueTip[:12])
	require.Empty(t, prep.Notes, "a mandatory item must not also be reported as background")
	require.Equal(t, float64(1),
		countedRecovery(t, reg, "ccw_bootstrap_workspace_recovered_total", "kind", "abort rescue"))
}

// TestPrepareWorkspace_FailedAbortUnusableRepoFallsBackToReclone pins the ONLY
// remaining path that deletes anything: the rescue itself could not be
// established, so there is no way to preserve the tree and no state left to
// reason about. Every rescue branch name is taken here, which is the cheap
// deterministic way to make the rescue impossible.
func TestPrepareWorkspace_FailedAbortUnusableRepoFallsBackToReclone(t *testing.T) {
	git := realGit(t)
	dest := initWorkRepo(t, git)
	for _, name := range bootstrap.AbortRescueBranchNamesForTest() {
		require.NoError(t, git(dest, "branch", name))
	}
	brokenRebaseState(t, dest)

	reg := prometheus.NewRegistry()
	prep, err := bootstrap.PrepareWorkspace(dest, git, nil, metrics.New(reg))
	require.NoError(t, err)
	require.True(t, prep.Wiped, "with no way to preserve the tree, a re-clone is the only state left")
	require.NoDirExists(t, dest)
	require.Equal(t, float64(1),
		countedRecovery(t, reg, "ccw_bootstrap_workspace_recovered_total", "kind", "unrecoverable"))
}

// TestPrepareWorkspace_FailedAbortCleanTreeRestoresHead: with nothing
// uncommitted there is nothing to preserve, so `reset --hard HEAD` discards
// exactly nothing and is the cheapest way back to a usable checkout.
func TestPrepareWorkspace_FailedAbortCleanTreeRestoresHead(t *testing.T) {
	git := realGit(t)
	dest := initWorkRepo(t, git)
	tip := commitFile(t, git, dest, "work.md", "committed\n", "work")
	require.NoError(t, os.MkdirAll(filepath.Join(dest, ".git", "rebase-merge"), 0o755))

	prep, err := bootstrap.PrepareWorkspace(dest, git, nil, nil)
	require.NoError(t, err)
	require.False(t, prep.Wiped)
	require.Equal(t, tip, gitOut(t, dest, "rev-parse", "HEAD"))
	require.Empty(t, prep.Required, "nothing was at risk, so nothing is required of the agent")
}

// TestResumeDirective_AbortRescueIsMandatory renders the whole boot and pins
// where the Phase C rescue lands: tier two, with the branch name and the sha,
// because a rescue branch nobody is told about is only marginally better than a
// reflog entry and it lives on this volume alone.
func TestResumeDirective_AbortRescueIsMandatory(t *testing.T) {
	git := realGit(t)
	remote := initBareRemote(t, git)
	ws := t.TempDir()
	p := workspaceParams(t, ws, remote, "tatara/task-alpha")
	require.NoError(t, bootstrap.Render(p, git))

	dest := bootstrap.RepoDir(ws, remote)
	taskTip := commitFile(t, git, dest, "work.md", "committed work\n", "real work")
	brokenRebaseState(t, dest)

	require.NoError(t, bootstrap.Render(p, git),
		"a session that could not be aborted is a job for the agent, never a boot failure")
	require.Equal(t, taskTip, gitOut(t, dest, "rev-parse", "refs/heads/tatara/task-alpha"))
	rescueTip := gitOut(t, dest, "rev-parse", "refs/heads/tatara-bootstrap-rescue-aborted")

	md := injectedGlobalClaudeMd(t, p)
	require.Contains(t, md, "hard requirement",
		"the agent must be told to look, not merely informed in passing")
	require.Contains(t, md, "tatara-bootstrap-rescue-aborted")
	require.Contains(t, md, rescueTip[:12], "the directive must name the sha the agent has to inspect")

	rescueAt := strings.Index(md, "tatara-bootstrap-rescue-aborted")
	requiredAt := strings.Index(md, "Hard requirement")
	require.Less(t, requiredAt, rescueAt, "the rescue belongs under the mandatory heading")
}

// TestResumeDirective_DetachedRescueIsSurfaced: the Phase D rescue branch is
// only a name on this volume, so the agent has to be told it exists and where
// to find it. It stays in the informational middle tier - nothing is required,
// the work is already on the branch it was made on - but it must carry both the
// branch name and the sha.
func TestResumeDirective_DetachedRescueIsSurfaced(t *testing.T) {
	git := realGit(t)
	remote := initBareRemote(t, git)
	ws := t.TempDir()
	p := workspaceParams(t, ws, remote, "tatara/task-alpha")
	require.NoError(t, bootstrap.Render(p, git))

	dest := bootstrap.RepoDir(ws, remote)
	commitFile(t, git, dest, "work.md", "committed work\n", "real work")
	require.NoError(t, git(dest, "checkout", "--detach", "HEAD"))
	writeFile(t, dest, "README.md", "work stranded on a detached HEAD\n")

	require.NoError(t, bootstrap.Render(p, git))
	rescueTip := gitOut(t, dest, "rev-parse", "refs/heads/tatara-bootstrap-rescue")

	md := injectedGlobalClaudeMd(t, p)
	require.Contains(t, md, "Workspace recovered from an interrupted session")
	require.Contains(t, md, "tatara-bootstrap-rescue",
		"a rescue branch the agent is never told about is barely better than a reflog entry")
	require.Contains(t, md, rescueTip[:12], "the directive must name the sha")
}
