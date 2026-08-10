package bootstrap

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
)

// This file implements the resumed-workspace contract: what bootstrap is
// allowed to do to a /workspace that outlived the pod which was using it.
//
// The motivating failure: /workspace was the container writable layer, an
// OOMKill destroyed six minutes of COMMITTED work, and the fix is a per-Task
// PersistentVolumeClaim. That volume turns every mid-operation state a killed
// pod can leave behind - a held index.lock, a half-applied merge, a detached
// HEAD, a local branch origin has never seen - from "impossible" into "the
// state most boots start from". Against the previous bootstrap a mounted volume
// was strictly worse than none: two paths in checkoutTaskBranch were outright
// fatal or silently destructive, and a boot failure now crash-loops the pod
// until the residency cap rather than costing one clone.
//
// One rule governs all of it. LOCAL COMMITS ARE AUTHORITATIVE AND ARE NEVER
// DISCARDED; the remote is authoritative for nothing. Precedence on the task
// branch runs uncommitted local work (committed, never stashed) > local commits
// > remote task-branch commits > base branch. Every operation here is therefore
// additive - merge, commit, or abort - because those are the only ones that
// cannot lose a side. Force-push is denied by the settings deny list, so a
// branch this code reset or rebased could never be published again: it would be
// stranded, and recovering it would need exactly the operation that is denied.
//
// The API constraint that shapes the implementation: GitRunner is
// func(dir string, args ...string) error and drops git's output deliberately
// (it can carry credential-helper expansions and tokenised remote URLs). Every
// check below is therefore an os.Stat or an exit-code probe, and the one place
// that needs a value back - the sha of a rescue commit - reads it out of
// .git/HEAD rather than widening the signature that every test double
// implements.

// WorkspacePrep is what PrepareWorkspace did to one repo checkout.
type WorkspacePrep struct {
	// Wiped reports that the checkout was unusable and has been removed. The
	// caller's own .git stat then misses and a normal fresh clone runs.
	Wiped bool
	// Notes are the repairs worth telling the AGENT about: an aborted merge or
	// rebase, work rescued into a commit. Platform noise (stale locks, a
	// half-cloned .git) is deliberately absent - it is counted and logged, and
	// surfacing it would spend prompt budget on something the agent cannot act
	// on.
	Notes []string
}

// PrepareWorkspace makes a possibly-inherited repo checkout safe to use, and
// must run BEFORE the .git stat that decides whether to clone - a checkout it
// wipes has to fall through to that clone in the same pass.
//
// The four phases run in this order because each depends on the last: locks
// must go before any git command can take the index, an in-progress operation
// must be aborted before the working tree can be judged, and the tree must be
// clean-or-committed before a branch can be checked out over it.
//
// It returns an error only when a wipe it decided to perform could not be
// carried out. Everything else is best-effort by design: a repair that fails
// leaves the checkout no worse than it was, and the phases after it still run.
func PrepareWorkspace(dest string, git GitRunner, log *slog.Logger, m *metrics.Metrics) (WorkspacePrep, error) {
	var prep WorkspacePrep
	if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
		return prep, nil // nothing inherited here; a fresh clone follows
	}

	// Phase A: is this a repo at all? A pod killed mid-clone leaves a .git that
	// git itself will not open, and every later phase would fail against it.
	// Conservative on purpose: only a DEFINITE failure of both probes wipes, and
	// wiping costs a re-clone while a false positive costs the volume's work.
	if !isUsableRepo(dest, git) {
		if err := os.RemoveAll(dest); err != nil {
			return prep, fmt.Errorf("remove unusable checkout %s: %w", dest, err)
		}
		prep.Wiped = true
		recordRecovery(log, m, recoveryInvalidGit, dest, "removed an unusable checkout, a fresh clone follows")
		return prep, nil
	}

	// Phase B: stale lock files.
	clearStaleLocks(dest, log, m)

	// Phase C: an operation the killed pod never finished.
	abortNotes, abortFailed := abortInProgress(dest, git, log, m)
	prep.Notes = append(prep.Notes, abortNotes...)

	// Phase D: the working tree.
	treeNotes, wiped, err := rescueWorkingTree(dest, git, log, m, abortFailed)
	if err != nil {
		return prep, err
	}
	if wiped {
		return WorkspacePrep{Wiped: true}, nil
	}
	prep.Notes = append(prep.Notes, treeNotes...)
	return prep, nil
}

// isUsableRepo reports whether git will operate on dest at all. Both probes are
// required: --git-dir alone succeeds inside a bare repo or a broken worktree
// link, and --is-inside-work-tree alone can be answered from an inherited
// environment variable.
func isUsableRepo(dest string, git GitRunner) bool {
	return git(dest, "rev-parse", "--is-inside-work-tree") == nil &&
		git(dest, "rev-parse", "--git-dir") == nil
}

// gitLockFiles are the top-level .git lock files a killed process leaves
// behind. Every one of them makes the corresponding git operation fail with
// "Another git process seems to be running", forever, because the process that
// held it died with the pod.
var gitLockFiles = []string{"index.lock", "shallow.lock", "HEAD.lock", "config.lock", "packed-refs.lock"}

// clearStaleLocks removes those locks unconditionally, plus any *.lock under
// .git/refs.
//
// Unconditional is the deliberate choice. There is no live git process on a
// freshly started pod - the wrapper is the first thing that runs and it has not
// forked git yet - so a lock found here is stale by construction. The obvious
// alternative, an mtime heuristic, is a trap on a persistent volume: the file's
// timestamp survived the pod that wrote it, so "recent" means nothing, and the
// heuristic's failure mode is leaving the repo permanently unusable.
// The whole sweep runs through an os.Root scoped at .git. The workspace is a
// volume the agent itself writes to, so a symlink planted under .git/refs is
// not a hypothetical: a root-scoped remove cannot be walked out of that
// directory, where a plain path-based one could be redirected between the walk
// and the unlink.
func clearStaleLocks(dest string, log *slog.Logger, m *metrics.Metrics) {
	root, err := os.OpenRoot(filepath.Join(dest, ".git"))
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()

	for _, name := range gitLockFiles {
		if root.Remove(name) == nil {
			recordLockCleared(log, m, dest, name, name)
		}
	}
	// Ref locks live at arbitrary depth (refs/heads/feature/x.lock). The metric
	// label is collapsed to "refs" so a branch name can never blow up the series
	// cardinality; the log line carries the actual path.
	_ = fs.WalkDir(root.FS(), "refs", func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil //nolint:nilerr // best-effort sweep: an unreadable entry must not stop the boot
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".lock") {
			return nil
		}
		if root.Remove(p) == nil {
			recordLockCleared(log, m, dest, "refs", p)
		}
		return nil
	})
}

// inProgressOp is one multi-step git operation, detected by the marker git
// writes into .git and undone by the command that restores the pre-operation
// state. fallback runs only when abort fails, and exists where git has two
// spellings for the same undo (a rebase-apply directory is also what `git am`
// uses, and `reset --merge` clears a merge whose MERGE_HEAD outlived its index).
type inProgressOp struct {
	marker   string
	kind     string
	abort    []string
	fallback []string
}

var inProgressOps = []inProgressOp{
	{"MERGE_HEAD", "merge", []string{"merge", "--abort"}, []string{"reset", "--merge"}},
	{"rebase-merge", "rebase", []string{"rebase", "--abort"}, nil},
	{"rebase-apply", "rebase", []string{"rebase", "--abort"}, []string{"am", "--abort"}},
	{"CHERRY_PICK_HEAD", "cherry pick", []string{"cherry-pick", "--abort"}, nil},
	{"REVERT_HEAD", "revert", []string{"revert", "--abort"}, nil},
	{"BISECT_LOG", "bisect", []string{"bisect", "reset"}, nil},
}

// abortInProgress undoes any operation the killed pod left half-applied.
//
// Abort, never "continue" and never leave it in place. An operation stopped
// mid-flight has an index git will not let anything else touch, and the turn
// finaliser's `git add -A` plus `git commit --no-verify` would happily publish
// its conflict markers to the task branch. Abort is also the only undo with a
// safe failure mode: it restores HEAD, the index and the working tree exactly,
// and it cannot lose a commit because it never moves a branch.
//
// Returns the agent-visible notes and whether any abort was left unfinished.
func abortInProgress(dest string, git GitRunner, log *slog.Logger, m *metrics.Metrics) (notes []string, abortFailed bool) {
	gitDir := filepath.Join(dest, ".git")
	for _, op := range inProgressOps {
		if _, err := os.Stat(filepath.Join(gitDir, op.marker)); err != nil {
			continue
		}
		err := git(dest, op.abort...)
		if err != nil && len(op.fallback) > 0 {
			err = git(dest, op.fallback...)
		}
		if err != nil {
			abortFailed = true
			if log != nil {
				log.Error("could not abort an interrupted git operation left by a killed pod",
					"action", "bootstrap_workspace", "repo", dest, "kind", op.kind)
			}
			continue
		}
		recordRecovery(log, m, "aborted "+op.kind, dest, "an interrupted "+op.kind+" was aborted")
		notes = append(notes, "an interrupted "+op.kind+" from the previous session was aborted; the working tree is back at the branch tip, and whatever that "+op.kind+" was meant to achieve has NOT been applied")
	}
	return notes, abortFailed
}

// rescueCommitMessage is the commit bootstrap makes over work the previous
// session had not committed.
const rescueCommitMessage = "tatara bootstrap: rescue uncommitted work from an interrupted session"

// rescueBranch names the commit made while HEAD was detached, so it stays
// reachable after the task branch is checked out over it.
const rescueBranch = "tatara-bootstrap-rescue"

// rescueWorkingTree deals with whatever the previous session left in the tree.
//
// Uncommitted tracked changes are COMMITTED, not stashed and not discarded. A
// stash is invisible to the agent - it does not appear in `git status`, the
// agent has no reason to look for it, and the turn finaliser's `git add -A`
// will never see it either, so stashing is a slower way of losing the work.
// unstageOversizedBlobs runs first, because `git add -A` would otherwise sweep
// a build artifact the previous session left behind onto the branch for every
// later turn to inherit (issue #131); that function exists for exactly this.
//
// Untracked files are left alone. Gitignored build output is the normal steady
// state of a persistent workspace, not damage, and touching it would make every
// resumed boot noisy for no gain.
//
// abortFailed is Phase C's escalation. When an abort could not be completed,
// git itself can no longer describe the repository, so no commit made on top of
// that state can be trusted. A clean tree is then restored with `reset --hard
// HEAD`, which discards nothing because there is nothing uncommitted to
// discard; a dirty one leaves a re-clone as the only state that can be reasoned
// about at all. That is the single place in this file where work can be lost,
// and it is bounded: everything already pushed is on origin, and the mid-turn
// safety net pushes the task branch every couple of minutes.
func rescueWorkingTree(dest string, git GitRunner, log *slog.Logger, m *metrics.Metrics, abortFailed bool) (notes []string, wiped bool, err error) {
	dirty := git(dest, "diff", "--quiet") != nil || git(dest, "diff", "--cached", "--quiet") != nil

	if abortFailed {
		if dirty {
			if rerr := os.RemoveAll(dest); rerr != nil {
				return nil, false, fmt.Errorf("remove unrecoverable checkout %s: %w", dest, rerr)
			}
			recordRecovery(log, m, recoveryUnrecoverable, dest, "an abort failed over a dirty tree, so the checkout was re-cloned")
			return nil, true, nil
		}
		_ = git(dest, "reset", "--hard", "HEAD")
		recordRecovery(log, m, recoveryResetAfterAbort, dest, "an abort failed over a clean tree, so the checkout was restored to HEAD")
		return nil, false, nil
	}

	// Detached HEAD is Phase E's problem: checking out the task branch
	// reattaches it. It is recorded rather than surfaced because the agent has
	// nothing to do about it - by the time the agent reads anything, it is
	// already fixed.
	detached := git(dest, "symbolic-ref", "-q", "HEAD") != nil
	if detached {
		recordRecovery(log, m, recoveryDetachedHead, dest, "the previous session left HEAD detached")
	}
	if !dirty {
		return nil, false, nil
	}

	if err := git(dest, "add", "-A"); err != nil {
		return nil, false, nil //nolint:nilerr // best-effort rescue: a failed add leaves the tree exactly as found
	}
	unstageOversizedBlobs(dest, git, log, m)
	if git(dest, "diff", "--cached", "--quiet") == nil {
		return nil, false, nil // everything staged was oversized and got unstaged
	}
	if err := git(dest, "commit", "--no-verify", "-m", rescueCommitMessage); err != nil {
		return nil, false, nil //nolint:nilerr // best-effort rescue: the tree is still on disk, unchanged
	}
	sha := headSHA(dest)
	note := "uncommitted work from the previous session was committed for you as `" + shortSHA(sha) + "` (" + rescueCommitMessage + "); review it and fold it into your own commits rather than assuming it is finished"
	if detached {
		// The commit was made on a detached HEAD, so checking out the task
		// branch would leave it reachable only through the reflog. A named
		// branch is additive and costs nothing; losing the commit is the one
		// outcome this whole file exists to prevent.
		if git(dest, "branch", "-f", rescueBranch) == nil {
			note += ". HEAD was detached at the time, so that commit is also kept on branch `" + rescueBranch + "`"
		}
	}
	recordRecovery(log, m, recoveryRescueCommit, dest, "uncommitted work was committed, sha "+sha)
	return []string{note}, false, nil
}

// headSHA reads the current commit out of .git rather than shelling out for it:
// GitRunner returns an exit code only, and widening it would touch every test
// double in the package. Returns "" when the ref is packed or unreadable, which
// only costs the note its sha.
func headSHA(dest string) string {
	head, err := os.ReadFile(filepath.Join(dest, ".git", "HEAD"))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(head))
	if ref, ok := strings.CutPrefix(s, "ref: "); ok {
		if !strings.HasPrefix(ref, "refs/") || strings.Contains(ref, "..") {
			return ""
		}
		b, rerr := os.ReadFile(filepath.Join(dest, ".git", filepath.FromSlash(ref))) //nolint:gosec // git's own HEAD pointer, constrained to refs/ under dest/.git
		if rerr != nil {
			return ""
		}
		s = strings.TrimSpace(string(b))
	}
	if len(s) < 7 {
		return ""
	}
	return s
}

// shortSHA renders a sha the way git logs do, and degrades to a readable
// placeholder when headSHA could not resolve one.
func shortSHA(sha string) string {
	if sha == "" {
		return "an unnamed commit"
	}
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// Recovery kinds, reported on ccw_bootstrap_workspace_recovered_total{kind}.
// Bounded by construction: every value is a constant here or an "aborted <op>"
// built from the fixed inProgressOps table.
const (
	recoveryInvalidGit      = "invalid git"
	recoveryUnrecoverable   = "unrecoverable"
	recoveryResetAfterAbort = "reset after abort"
	recoveryDetachedHead    = "detached head"
	recoveryRescueCommit    = "rescue commit"
	recoveryWrongTask       = "wrong task"
	recoveryRepoSetChanged  = "repo set changed"
)

func recordRecovery(log *slog.Logger, m *metrics.Metrics, kind, dest, msg string) {
	if m != nil {
		m.BootstrapWorkspaceRecovered.WithLabelValues(kind).Inc()
	}
	if log != nil {
		log.Info(msg, "action", "bootstrap_workspace", "repo", dest, "kind", kind)
	}
}

func recordLockCleared(log *slog.Logger, m *metrics.Metrics, dest, label, path string) {
	if m != nil {
		m.BootstrapLockCleared.WithLabelValues(label).Inc()
	}
	if log != nil {
		log.Info("removed a stale git lock left by a killed pod", "action", "bootstrap_workspace",
			"repo", dest, "lock", label, "path", path)
	}
}

// workspaceIdentityFile records which Task a persistent workspace belongs to.
// It lives at the workspace ROOT, outside every repo working tree, for the same
// reason the injected CLAUDE.md and .mcp.json do: the turn finaliser's
// `git add -A` runs inside a repo directory and can never reach it.
const workspaceIdentityFile = ".tatara-workspace.json"

// workspaceContractVersion is the shape of that file. It is written so a future
// change to the contract can recognise, rather than guess at, a volume written
// by an older wrapper.
const workspaceContractVersion = 1

type workspaceIdentity struct {
	ContractVersion int      `json:"contractVersion"`
	TaskName        string   `json:"taskName"`
	ProjectName     string   `json:"projectName"`
	RepoURLs        []string `json:"repoURLs"`
}

// workspaceRepoURLs is the repo set this Render is about to materialise, in
// declaration order.
func workspaceRepoURLs(p Params) []string {
	if len(p.Repos) > 0 {
		out := make([]string, 0, len(p.Repos))
		for _, r := range p.Repos {
			out = append(out, r.URL)
		}
		return out
	}
	if p.RepoURL != "" {
		return []string{p.RepoURL}
	}
	return nil
}

// claimWorkspace decides whether an inherited workspace may be used for this
// Task, and returns the workspace-level notes the agent should see.
//
// A per-Task PersistentVolumeClaim cannot legitimately hold another Task's
// tree, so a taskName mismatch means the volume was rebound and everything on
// it belongs to work this pod knows nothing about. The whole workspace is wiped
// and treated as fresh: guessing which parts are reusable is strictly worse
// than re-cloning, because a wrong guess hands the agent another Task's code as
// though it were its own. It is silent - the agent has nothing to act on, and a
// wiped workspace is indistinguishable from a first boot.
//
// A changed repo SET is not a mismatch. New repos are cloned normally, and
// repos no longer in the Project are LEFT on disk: a removed repo may hold
// uncommitted work, and deciding to delete it is not bootstrap's call. That one
// is surfaced, because the agent will otherwise find a directory the Project
// does not mention and have no way to know why.
//
// Nothing is wiped when either taskName is empty. An empty name proves nothing,
// and a destructive action taken on no evidence is the worst trade here.
func claimWorkspace(p Params, log *slog.Logger, m *metrics.Metrics) ([]string, error) {
	prev, ok := readWorkspaceIdentity(p.Workspace)
	if !ok {
		return nil, nil
	}
	if prev.TaskName != "" && p.TaskName != "" && prev.TaskName != p.TaskName {
		if err := os.RemoveAll(p.Workspace); err != nil {
			return nil, fmt.Errorf("wipe workspace claimed by another task: %w", err)
		}
		if err := os.MkdirAll(p.Workspace, 0o755); err != nil {
			return nil, fmt.Errorf("recreate wiped workspace: %w", err)
		}
		recordRecovery(log, m, recoveryWrongTask, p.Workspace, "workspace volume held another task's tree and was wiped")
		return nil, nil
	}
	added, removed := diffStrings(prev.RepoURLs, workspaceRepoURLs(p))
	if len(added) == 0 && len(removed) == 0 {
		return nil, nil
	}
	recordRecovery(log, m, recoveryRepoSetChanged, p.Workspace, "the project repo set changed since this workspace was last used")
	var notes []string
	for _, u := range added {
		notes = append(notes, "`"+u+"` was added to this project since the last session and has been cloned in for the first time")
	}
	for _, u := range removed {
		notes = append(notes, "`"+u+"` is no longer part of this project. Its checkout has been LEFT on disk in case it holds uncommitted work; bootstrap will not update it again, so do not treat it as current")
	}
	return notes, nil
}

// diffStrings returns what is in want but not have, and what is in have but not
// want, each in the order of its own slice.
func diffStrings(have, want []string) (added, removed []string) {
	inHave := map[string]bool{}
	for _, s := range have {
		inHave[s] = true
	}
	inWant := map[string]bool{}
	for _, s := range want {
		inWant[s] = true
	}
	for _, s := range want {
		if !inHave[s] {
			added = append(added, s)
		}
	}
	for _, s := range have {
		if !inWant[s] {
			removed = append(removed, s)
		}
	}
	return added, removed
}

func readWorkspaceIdentity(workspace string) (workspaceIdentity, bool) {
	b, err := os.ReadFile(filepath.Join(workspace, workspaceIdentityFile))
	if err != nil {
		return workspaceIdentity{}, false
	}
	var id workspaceIdentity
	if json.Unmarshal(b, &id) != nil {
		return workspaceIdentity{}, false
	}
	return id, true
}

// writeWorkspaceIdentity stamps the workspace at the end of a successful
// Render. Writing it earlier would claim a volume for a Task whose bootstrap
// then failed, and the next boot would read that claim as proof the tree is
// this Task's own.
func writeWorkspaceIdentity(p Params) error {
	id := workspaceIdentity{
		ContractVersion: workspaceContractVersion,
		TaskName:        p.TaskName,
		ProjectName:     p.ProjectName,
		RepoURLs:        workspaceRepoURLs(p),
	}
	b, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal workspace identity: %w", err)
	}
	if err := os.WriteFile(filepath.Join(p.Workspace, workspaceIdentityFile), b, 0o644); err != nil {
		return fmt.Errorf("write workspace identity: %w", err)
	}
	return nil
}
