package bootstrap

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
