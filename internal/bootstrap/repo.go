package bootstrap

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
)

// configureGit sets the global commit identity and, when a token is present, a
// credential helper so both the clone and the agent's later push authenticate
// against a private repo. The helper reads $GIT_TOKEN at invocation time, so the
// token is never written into .gitconfig or a remote URL.
func configureGit(p Params, git GitRunner) error {
	if p.GitUserName != "" {
		if err := git("", "config", "--global", "user.name", p.GitUserName); err != nil {
			return err
		}
	}
	if p.GitUserEmail != "" {
		if err := git("", "config", "--global", "user.email", p.GitUserEmail); err != nil {
			return err
		}
	}
	if p.GitToken != "" {
		helper := `!f() { echo username=x-access-token; echo "password=$GIT_TOKEN"; }; f`
		if err := git("", "config", "--global", "credential.helper", helper); err != nil {
			return err
		}
	}
	return nil
}

// RepoDir returns the on-disk directory a single-repo clone lives in:
// workspace/<namespacePath(repoURL)>. It mirrors the single-repo clone target
// in Render. Returns "" when the URL yields no valid namespace (the same
// condition under which Render refuses to clone), so callers can skip safely.
func RepoDir(workspace, repoURL string) string {
	ns := namespacePath(repoURL)
	if ns == "" || filepath.Clean(filepath.Join(workspace, ns)) == filepath.Clean(workspace) {
		return ""
	}
	return filepath.Join(workspace, ns)
}

// MaxStagedBlobBytes is the size above which CommitAndPush refuses to stage a
// file. Only build artifacts get this big in an agent workspace (issue #131:
// `git add -A` swept a 15.7 MB `wrapper` ELF onto a task branch, and the
// `--no-verify` below bypassed the pre-commit check-added-large-files hook
// that exists to stop exactly that). Committed once, it is inherited by every
// later turn on that branch. Source files never approach this. A var, not a
// const, so tests can lower it without writing 10 MB to disk.
var MaxStagedBlobBytes int64 = 10 << 20

// CommitAndPush stages all changes, unstages any oversized build artifact,
// and when something is still staged commits and pushes the branch to origin.
// A clean tree is left untouched: nothing is committed or pushed, so no empty
// remote branch is created. Returns pushed=true only when a commit was made and
// the push succeeded. log and m may be nil (the guard then runs silently).
func CommitAndPush(dir, branch, message string, git GitRunner, log *slog.Logger, m *metrics.Metrics) (pushed bool, err error) {
	if err := git(dir, "add", "-A"); err != nil {
		return false, err
	}
	unstageOversizedBlobs(dir, git, log, m)
	// `diff --cached --quiet` exits zero (nil) when the tree is clean.
	if git(dir, "diff", "--cached", "--quiet") == nil {
		return false, nil
	}
	if err := git(dir, "commit", "--no-verify", "-m", message); err != nil {
		return false, err
	}
	if err := git(dir, "push", "--no-verify", "-u", "origin", branch); err != nil {
		return false, err
	}
	return true, nil
}

// benignPushFailures are git push outcomes the mid-turn safety net must not
// treat as failures. "everything up-to-date" is the ordinary no-new-commit
// tick; the rejection forms mean the remote moved, which the next turn's
// resume reconcile handles (see reconcileWithBase).
//
// Note that the production GitRunner deliberately drops git's combined output
// (it can carry credential-helper expansions), so in the pod only the
// zero-exit forms are recognised here. That is fine: PushOnly's caller treats
// EVERY error as a warning it counts and logs, never as something fatal. This
// classification is what keeps the counter honest for any runner that does
// surface the message.
//
// PRIVATE TO PushOnly ON PURPOSE. CommitAndPush classifies nothing, and the
// asymmetry is a decision rather than drift (issue #167). What a shared
// predicate would buy is loop control - "a benign rejection must not kill the
// fan-out" - and CommitAndPushAll already never aborts on any error class, so
// there is nothing left for it to decide. What remains is a log/metric nuance
// that cannot fire in the pod anyway, for the reason two paragraphs up, so
// sharing the list would add a second place to drift in exchange for no
// production behaviour at all.
//
// Note the two paths return errors for opposite reasons, which is the other
// half of why one predicate does not fit both: PushOnly returns nil on a benign
// match because its caller has nothing else to do with the error, while
// CommitAndPush returns every error because its caller now uses it purely as a
// report that costs the other repos nothing. "Benign" here must never mean
// "pretend it pushed" - at turn end a rejected push is exactly the case where
// the agent's commit is not on origin.
var benignPushFailures = []string{
	"everything up-to-date",
	"everything up to date",
	"non-fast-forward",
	"fetch first",
}

// AutoPushEnabled is THE predicate for "this pod auto-pushes its task branch
// mid-turn". Both consumers must call it and neither may re-derive it:
// safetyPusher.Enabled decides whether the ticker runs, and autoPushDirective
// decides whether the agent is told the ticker runs. Duplicating the condition
// is how the two drift, and the drift is the bug - the pod then promises the
// agent a push that never happens, and the agent skips an amend it was
// actually free to make.
//
// taskBranch == "" is the read-only review checkout: the branch is someone
// else's pull request head and must never be pushed. interval <= 0 is the
// explicit off switch (BRANCH_PUSH_INTERVAL_SECONDS=0).
func AutoPushEnabled(taskBranch string, interval time.Duration) bool {
	return taskBranch != "" && interval > 0
}

// PushOnly pushes branch to origin and does nothing else. It is the mid-turn
// safety net: `git add` and `git commit` are deliberately absent, so it never
// takes .git/index.lock out from under the agent's own concurrent git calls
// and can never publish a half-edited tree or a conflict marker. The branch
// only ever advances by commits the agent chose to make.
//
// Why it exists: /workspace is the container writable layer with no volume, so
// a commit that has not reached origin dies with the pod. Agent pod
// imp-mtg-mtg-decks-i37 was OOMKilled six minutes after committing, and the
// commit was gone. CommitAndPush at turn end is too coarse for a 42-minute
// turn.
func PushOnly(dir, branch string, git GitRunner) error {
	err := git(dir, "push", "--no-verify", "--quiet", "origin", branch)
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	for _, benign := range benignPushFailures {
		if strings.Contains(msg, benign) {
			return nil
		}
	}
	return err
}

// unstageOversizedBlobs walks the repo working tree and unstages every file
// over MaxStagedBlobBytes, so a build artifact the agent left behind never
// reaches the commit. `git reset -- <path>` on a path that was not staged (an
// ignored file) is a harmless no-op, so no gitignore lookup is needed.
// Best-effort by design: a walk failure must never block the agent's push, and
// the pre-existing tests drive CommitAndPush against directories that do not
// exist at all.
func unstageOversizedBlobs(dir string, git GitRunner, log *slog.Logger, m *metrics.Metrics) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // best-effort: skip unreadable entries, never abort the push
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.Size() <= MaxStagedBlobBytes {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if gerr := git(dir, "reset", "-q", "--", rel); gerr != nil {
			return nil
		}
		if m != nil {
			m.CommitOversizedBlobSkipped.Inc()
		}
		if log != nil {
			log.Warn("refused to stage oversized file: build artifacts must not be committed to the task branch",
				"action", "commit_oversized_blob", "path", rel, "size_bytes", info.Size(), "limit_bytes", MaxStagedBlobBytes)
		}
		return nil
	})
}

// CommitAndPushAll runs CommitAndPush in each repo dir under workspace. It
// returns the Name of every repo that had a diff and pushed, the Name of every
// repo whose commit/push failed, and every failure joined into one error.
//
// EVERY REPO IS ATTEMPTED, UNCONDITIONALLY. The loop used to return on the first
// error, which meant every repo after it never even reached `git add -A` - and
// the mid-turn safety net cannot cover for that, because PushOnly deliberately
// omits add/commit and a tree that was never committed has nothing to push.
// /workspace is the container writable layer with no volume, so those edits died
// with the pod. The repos are independent: nothing about repo A's push makes
// repo B's commit wrong. This is the same per-repo, best-effort stance the clone
// path and unstageOversizedBlobs already take.
//
// The error is therefore a REPORT, not loop control. It is still returned, still
// counted and still logged - the failure did not become acceptable, it just
// stopped costing the other repos their commits. failed is the same information
// in a form the operator can act on: a short pushed list is indistinguishable
// from a turn with nothing to push, and the difference is whether work was lost.
//
// KNOWN LIMITATION, deliberate: failed describes THIS turn, like pushed does. A
// repo that committed and then failed to push has a clean index on the next
// turn, so CommitAndPush short-circuits, the repo appears in neither list, and
// the operator's status field is cleared while the commit is still local. The
// mid-turn safety net covers the common case - PushOnly pushes every repo each
// interval regardless of tree state, so a transient rejection self-heals - but a
// persistent one (auth, branch protection) does not. Retrying an unpushed commit
// from the turn-end path means comparing against the remote-tracking ref, which
// is a different change from this one and belongs with reconcileWithBase.
func CommitAndPushAll(workspace string, repos []RepoSpec, branch, message string, git GitRunner, log *slog.Logger, m *metrics.Metrics) (pushed []string, failed []string, err error) {
	for _, r := range repos {
		ns := namespacePath(r.URL)
		if ns == "" || filepath.Clean(filepath.Join(workspace, ns)) == filepath.Clean(workspace) {
			// No valid namespace: skip, or we would operate on the workspace root.
			// Say so. The clone path skipped this same spec for this same reason,
			// so there is no tree here and nothing to lose - which is why it does
			// NOT join failed: a failed entry makes the operator's handoff note
			// assert lost work in a repo that was never on disk. But a repo that
			// drops out of a nine-wide fan-out in silence is the same blind spot
			// this reporting exists to close, one step earlier.
			if log != nil {
				log.Warn("skipping repo with no derivable namespace: it was never cloned and is in neither the pushed nor the failed list",
					"action", "repo_skipped", "repo", r.Name, "url", r.URL)
			}
			continue
		}
		dir := filepath.Join(workspace, ns)
		// TATARA_REPOS is unmarshalled with no validation, so a RepoSpec can carry
		// a URL with no Name. Fall back to the last segment of the namespace we
		// just derived from that same URL rather than ever appending "".
		//
		// filepath.Base, not a slice at the last "/": namespacePath trims a ".git"
		// suffix AFTER joining, so a URL ending "/.git" yields "owner/repo/" and
		// the slice would hand back the "" this exists to prevent. It is also how
		// primaryRepoName derives the single-repo name, and the two must not
		// disagree about the same URL.
		name := r.Name
		if name == "" {
			name = filepath.Base(ns)
		}
		ok, perr := CommitAndPush(dir, branch, message, git, log, m)
		if perr != nil {
			failed = append(failed, name)
			err = errors.Join(err, fmt.Errorf("commit/push %s: %w", name, perr))
			continue
		}
		if ok {
			pushed = append(pushed, name)
		}
	}
	return pushed, failed, err
}
