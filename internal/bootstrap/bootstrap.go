package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
)

// RepoSpec is one Project repo to clone into the workspace.
type RepoSpec struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Branch string `json:"branch"`
}

type Params struct {
	HomeDir, Workspace              string
	GlobalClaudeMd, ProjectClaudeMd string
	BaseMCP                         []byte
	MCPOverlayDir                   string
	SkillsSrc                       []string
	HookCommand                     string
	AllowedTools                    []string
	EnableAllMCP                    bool
	PermissionMode                  string
	AnthropicAPIKey                 string // used to seed customApiKeyResponses (last 20 chars)
	RepoURL, RepoBranch             string
	GitToken                        string // private-repo auth for clone + the agent's push (read from $GIT_TOKEN at runtime, never written to disk)
	GitUserName, GitUserEmail       string // commit identity for the agent
	TaskBranch                      string // work branch the operator opens the PR from; checked out after clone
	Repos                           []RepoSpec
}

// GitRunner runs a git subcommand in dir; injected for testability.
type GitRunner func(dir string, args ...string) error

// checkoutTaskBranch checks out taskBranch in dir. If the branch already
// exists on the remote (ls-remote --exit-code returns nil), it resumes:
// unshallows the clone (best-effort), fetches the remote branch, then checks
// out with -B <branch> FETCH_HEAD so the agent has its prior commits and a
// full history to rebase against. If the branch does not exist, it falls back
// to the fresh-task path: plain "checkout -b <branch>".
func checkoutTaskBranch(dir, taskBranch string, git GitRunner) error {
	branchExists := git(dir, "ls-remote", "--exit-code", "--heads", "origin", taskBranch) == nil
	if branchExists {
		// Best-effort unshallow so rebase has a merge base; ignore error (already complete).
		_ = git(dir, "fetch", "--unshallow", "origin")
		if err := git(dir, "fetch", "origin", taskBranch); err != nil {
			return fmt.Errorf("fetch remote task branch %s: %w", taskBranch, err)
		}
		return git(dir, "checkout", "-B", taskBranch, "FETCH_HEAD")
	}
	return git(dir, "checkout", "-b", taskBranch)
}

func Render(p Params, git GitRunner) error {
	claudeHome := filepath.Join(p.HomeDir, ".claude")
	if err := os.MkdirAll(claudeHome, 0o755); err != nil {
		return fmt.Errorf("mkdir claude home: %w", err)
	}
	if err := os.MkdirAll(p.Workspace, 0o755); err != nil {
		return fmt.Errorf("mkdir workspace: %w", err)
	}
	if len(p.Repos) > 0 {
		if err := configureGit(p, git); err != nil {
			return err
		}
		for _, r := range p.Repos {
			ns := namespacePath(r.URL)
			// Guard: an empty namespace path would resolve to the workspace root,
			// overwriting session config files (.mcp.json, CLAUDE.md, settings).
			if ns == "" || filepath.Clean(filepath.Join(p.Workspace, ns)) == filepath.Clean(p.Workspace) {
				if r.URL == p.RepoURL {
					return fmt.Errorf("cannot derive namespace path from URL %q: would clone into workspace root", r.URL)
				}
				continue // non-primary: skip silently
			}
			dest := filepath.Join(p.Workspace, ns)
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				if r.URL == p.RepoURL {
					return fmt.Errorf("mkdir parent for primary repo %s: %w", r.Name, err)
				}
				continue // non-primary parent-dir failure: skip
			}
			args := []string{"clone", "--depth", "1"}
			if r.Branch != "" {
				args = append(args, "--branch", r.Branch)
			}
			args = append(args, r.URL, dest)
			if err := git(p.Workspace, args...); err != nil {
				if r.URL == p.RepoURL {
					return fmt.Errorf("clone primary repo %s: %w", r.Name, err)
				}
				continue // non-primary clone failure: skip
			}
			if p.TaskBranch != "" {
				if err := checkoutTaskBranch(dest, p.TaskBranch, git); err != nil {
					if r.URL == p.RepoURL {
						return err
					}
				}
			}
		}
	} else if p.RepoURL != "" {
		if err := configureGit(p, git); err != nil {
			return err
		}
		if err := cloneRepo(p, git); err != nil {
			return err
		}
		if p.TaskBranch != "" {
			if err := checkoutTaskBranch(p.Workspace, p.TaskBranch, git); err != nil {
				return err
			}
		}
	}
	if err := writeIfSet(filepath.Join(p.Workspace, "CLAUDE.md"), p.ProjectClaudeMd); err != nil {
		return err
	}
	if err := writeIfSet(filepath.Join(claudeHome, "CLAUDE.md"), p.GlobalClaudeMd); err != nil {
		return err
	}
	if err := mergeMCP(p); err != nil {
		return err
	}
	if err := writeSettings(p, claudeHome); err != nil {
		return err
	}
	if err := writeClaudeJSON(p); err != nil {
		return err
	}
	return installSkills(p)
}

func writeIfSet(path, content string) error {
	if content == "" {
		return nil
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
