package bootstrap

import (
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"

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

// CommitAndPushAll runs CommitAndPush in each repo dir under workspace and
// returns the Name of every repo that had a diff and pushed, so the caller can
// report the exact touched-repo set to the operator.
func CommitAndPushAll(workspace string, repos []RepoSpec, branch, message string, git GitRunner, log *slog.Logger, m *metrics.Metrics) (pushed []string, err error) {
	for _, r := range repos {
		ns := namespacePath(r.URL)
		if ns == "" || filepath.Clean(filepath.Join(workspace, ns)) == filepath.Clean(workspace) {
			continue // no valid namespace: skip to avoid operating on the workspace root
		}
		dir := filepath.Join(workspace, ns)
		ok, perr := CommitAndPush(dir, branch, message, git, log, m)
		if perr != nil {
			return pushed, fmt.Errorf("commit/push %s: %w", r.Name, perr)
		}
		if ok {
			pushed = append(pushed, r.Name)
		}
	}
	return pushed, nil
}
