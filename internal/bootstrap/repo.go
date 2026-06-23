package bootstrap

import (
	"fmt"
	"path/filepath"
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

// CommitAndPush stages all changes, and when something is staged commits and
// pushes the branch to origin. A clean tree is left untouched: nothing is
// committed or pushed, so no empty remote branch is created. Returns pushed=true
// only when a commit was made and the push succeeded.
func CommitAndPush(dir, branch, message string, git GitRunner) (pushed bool, err error) {
	if err := git(dir, "add", "-A"); err != nil {
		return false, err
	}
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

// CommitAndPushAll runs CommitAndPush in each repo dir under workspace and
// returns the Name of every repo that had a diff and pushed, so the caller can
// report the exact touched-repo set to the operator.
func CommitAndPushAll(workspace string, repos []RepoSpec, branch, message string, git GitRunner) (pushed []string, err error) {
	for _, r := range repos {
		ns := namespacePath(r.URL)
		if ns == "" || filepath.Clean(filepath.Join(workspace, ns)) == filepath.Clean(workspace) {
			continue // no valid namespace: skip to avoid operating on the workspace root
		}
		dir := filepath.Join(workspace, ns)
		ok, perr := CommitAndPush(dir, branch, message, git)
		if perr != nil {
			return pushed, fmt.Errorf("commit/push %s: %w", r.Name, perr)
		}
		if ok {
			pushed = append(pushed, r.Name)
		}
	}
	return pushed, nil
}
