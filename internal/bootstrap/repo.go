package bootstrap

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

// CommitAndPush stages all changes, commits them when something is staged, and
// pushes branch to origin. It enforces the branch+commit+push the agent is
// asked to do but does not reliably perform, so the operator's write-back finds
// the branch. Push runs even with nothing new so the remote branch exists.
func CommitAndPush(dir, branch, message string, git GitRunner) error {
	if err := git(dir, "add", "-A"); err != nil {
		return err
	}
	// `diff --cached --quiet` exits non-zero when there are staged changes.
	if git(dir, "diff", "--cached", "--quiet") != nil {
		if err := git(dir, "commit", "-m", message); err != nil {
			return err
		}
	}
	return git(dir, "push", "-u", "origin", branch)
}

// cloneRepo shallow-clones RepoURL@RepoBranch into the workspace.
func cloneRepo(p Params, git GitRunner) error {
	args := []string{"clone", "--depth", "1"}
	if p.RepoBranch != "" {
		args = append(args, "--branch", p.RepoBranch)
	}
	args = append(args, p.RepoURL, p.Workspace)
	if err := git(p.Workspace, args...); err != nil {
		return err
	}
	return nil
}
