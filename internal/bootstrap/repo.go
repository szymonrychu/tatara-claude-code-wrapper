package bootstrap

// configureGit sets the global commit identity and, when a token is present, a
// credential helper so both the clone and the agent's later push authenticate
// against a private repo. The helper reads $GIT_TOKEN at invocation time, so the
// token is never written into .gitconfig or a remote URL.
func configureGit(p Params, git GitRunner) error {
	if p.GitUserName != "" {
		if err := git("config", "--global", "user.name", p.GitUserName); err != nil {
			return err
		}
	}
	if p.GitUserEmail != "" {
		if err := git("config", "--global", "user.email", p.GitUserEmail); err != nil {
			return err
		}
	}
	if p.GitToken != "" {
		helper := `!f() { echo username=x-access-token; echo "password=$GIT_TOKEN"; }; f`
		if err := git("config", "--global", "credential.helper", helper); err != nil {
			return err
		}
	}
	return nil
}

// cloneRepo shallow-clones RepoURL@RepoBranch into the workspace.
func cloneRepo(p Params, git GitRunner) error {
	args := []string{"clone", "--depth", "1"}
	if p.RepoBranch != "" {
		args = append(args, "--branch", p.RepoBranch)
	}
	args = append(args, p.RepoURL, p.Workspace)
	if err := git(args...); err != nil {
		return err
	}
	return nil
}
