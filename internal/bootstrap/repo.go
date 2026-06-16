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

// CommitAndPush stages all changes, and when something is staged commits and
// pushes the branch to origin. A clean tree is left untouched: nothing is
// committed and nothing is pushed, so no empty remote branch is created.
func CommitAndPush(dir, branch, message string, git GitRunner) error {
	if err := git(dir, "add", "-A"); err != nil {
		return err
	}
	// `diff --cached --quiet` exits zero (nil) when the tree is clean.
	if git(dir, "diff", "--cached", "--quiet") == nil {
		return nil
	}
	if err := git(dir, "commit", "--no-verify", "-m", message); err != nil {
		return err
	}
	return git(dir, "push", "--no-verify", "-u", "origin", branch)
}

// CommitAndPushAll runs CommitAndPush in each repo dir under workspace.
func CommitAndPushAll(workspace string, repos []RepoSpec, branch, message string, git GitRunner) error {
	for _, r := range repos {
		ns := namespacePath(r.URL)
		if ns == "" || filepath.Clean(filepath.Join(workspace, ns)) == filepath.Clean(workspace) {
			continue // no valid namespace: skip to avoid operating on the workspace root
		}
		dir := filepath.Join(workspace, ns)
		if err := CommitAndPush(dir, branch, message, git); err != nil {
			return fmt.Errorf("commit/push %s: %w", r.Name, err)
		}
	}
	return nil
}
