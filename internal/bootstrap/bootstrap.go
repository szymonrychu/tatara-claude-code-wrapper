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
			dest := filepath.Join(p.Workspace, r.Name)
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
				if err := git(dest, "checkout", "-b", p.TaskBranch); err != nil {
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
			if err := git(p.Workspace, "checkout", "-b", p.TaskBranch); err != nil {
				return err
			}
		}
		if err := excludeWorkspaceConfig(p.Workspace); err != nil {
			return err
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
