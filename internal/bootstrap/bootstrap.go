package bootstrap

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
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

	// Optional: provide structured logging and metrics (rules 12+13).
	// When nil, Render/CommitAndPush run silently without emitting log lines or metrics.
	Log *slog.Logger
	M   *metrics.Metrics
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
		// Unshallow so the rebase against origin/<default> has a merge base. The
		// clone is always --depth 1 (cloneRepo / the multi-repo clone above), so
		// unshallow always applies and a failure here is real (network), not the
		// benign "already complete" case -- propagate it rather than proceed with a
		// shallow clone whose rebase would fail with no merge base.
		if err := git(dir, "fetch", "--unshallow", "origin"); err != nil {
			return fmt.Errorf("unshallow for resume of %s: %w", taskBranch, err)
		}
		if err := git(dir, "fetch", "origin", taskBranch); err != nil {
			return fmt.Errorf("fetch remote task branch %s: %w", taskBranch, err)
		}
		return git(dir, "checkout", "-B", taskBranch, "FETCH_HEAD")
	}
	return git(dir, "checkout", "-b", taskBranch)
}

func Render(p Params, git GitRunner) error {
	renderStart := time.Now()
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
		for i, r := range p.Repos {
			// Primary repo is always Repos[0], identified by position rather than
			// URL comparison so that an empty p.RepoURL never masks a clone failure.
			isPrimary := i == 0
			ns := namespacePath(r.URL)
			// Guard: an empty namespace path would resolve to the workspace root,
			// overwriting session config files (.mcp.json, CLAUDE.md, settings).
			if ns == "" || filepath.Clean(filepath.Join(p.Workspace, ns)) == filepath.Clean(p.Workspace) {
				if isPrimary {
					return fmt.Errorf("cannot derive namespace path from URL %q: would clone into workspace root", r.URL)
				}
				continue // non-primary: skip silently
			}
			dest := filepath.Join(p.Workspace, ns)
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				if isPrimary {
					return fmt.Errorf("mkdir parent for primary repo %s: %w", r.Name, err)
				}
				continue // non-primary parent-dir failure: skip
			}
			cloneStart := time.Now()
			// Skip clone when the repo is already present (pod restart with persistent workspace).
			if _, statErr := os.Stat(filepath.Join(dest, ".git")); os.IsNotExist(statErr) {
				args := []string{"clone", "--depth", "1"}
				if r.Branch != "" {
					args = append(args, "--branch", r.Branch)
				}
				args = append(args, r.URL, dest)
				if err := git(p.Workspace, args...); err != nil {
					if p.M != nil {
						p.M.BootstrapCloneTotal.WithLabelValues("fail").Inc()
					}
					if isPrimary {
						return fmt.Errorf("clone primary repo %s: %w", r.Name, err)
					}
					continue // non-primary clone failure: skip
				}
			}
			action := "clone"
			if p.TaskBranch != "" {
				// Surface checkout/resume failures for ALL repos (not just the
				// primary): a secondary repo silently left on the wrong branch
				// would make the agent commit the wrong state. Fail loud so the
				// operator retries the run.
				if err := checkoutTaskBranch(dest, p.TaskBranch, git); err != nil {
					if p.M != nil {
						p.M.BootstrapCloneTotal.WithLabelValues("fail").Inc()
					}
					return fmt.Errorf("checkout task branch in %s: %w", r.Name, err)
				}
				if branchExists := git(dest, "ls-remote", "--exit-code", "--heads", "origin", p.TaskBranch) == nil; branchExists {
					action = "resume"
				}
			}
			if p.M != nil {
				p.M.BootstrapCloneTotal.WithLabelValues("ok").Inc()
			}
			if p.Log != nil {
				p.Log.Info("repo cloned", "action", action, "repo", r.Name, "branch", r.Branch,
					"task_branch", p.TaskBranch, "duration_ms", time.Since(cloneStart).Milliseconds())
			}
		}
	} else if p.RepoURL != "" {
		if err := configureGit(p, git); err != nil {
			return err
		}
		cloneStart := time.Now()
		if err := cloneRepo(p, git); err != nil {
			if p.M != nil {
				p.M.BootstrapCloneTotal.WithLabelValues("fail").Inc()
			}
			return err
		}
		action := "clone"
		if p.TaskBranch != "" {
			if err := checkoutTaskBranch(p.Workspace, p.TaskBranch, git); err != nil {
				if p.M != nil {
					p.M.BootstrapCloneTotal.WithLabelValues("fail").Inc()
				}
				return err
			}
			if branchExists := git(p.Workspace, "ls-remote", "--exit-code", "--heads", "origin", p.TaskBranch) == nil; branchExists {
				action = "resume"
			}
		}
		if p.M != nil {
			p.M.BootstrapCloneTotal.WithLabelValues("ok").Inc()
		}
		if p.Log != nil {
			p.Log.Info("repo cloned", "action", action, "repo", p.RepoURL, "branch", p.RepoBranch,
				"task_branch", p.TaskBranch, "duration_ms", time.Since(cloneStart).Milliseconds())
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
	if err := installSkills(p); err != nil {
		return err
	}
	if p.M != nil {
		p.M.BootstrapDuration.Observe(time.Since(renderStart).Seconds())
	}
	return nil
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
