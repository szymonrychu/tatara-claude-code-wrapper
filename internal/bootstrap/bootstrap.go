package bootstrap

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	GrafanaMCPURL                   string
	SerenaMCPURL                    string
	// ExtraMCPServers carries TATARA_EXTRA_MCP_SERVERS (Phase 2 contract): a
	// compact JSON array [{"name","url","type"}] of Project-scoped MCP servers
	// merged into .mcp.json after the overlay dir and before the platform
	// servers below, so grafana/serena/tatara always win on a name collision.
	ExtraMCPServers []byte
	SkillsSrc       []string
	SkillProfile    string // TATARA_SKILL_PROFILE; empty = install all
	SkillsRepo      string // TATARA_SKILLS_REPO; URL to clone at boot
	SkillsRef       string // TATARA_SKILLS_REF; git ref for the clone
	SkillsCloneDir  string // directory where the skills repo is cloned
	// AgentsSrc lists directories whose top-level *.md files are installed
	// into <workspace>/.claude/agents: the typed subagent definitions shipped
	// by tatara-agent-skills (explorer/tester/builder/architect, model
	// tiering baked into each file's own frontmatter - see task-kind
	// redesign Decision 6). Derived from the same skills-repo clone as
	// SkillsSrc; not profile-gated (see agents.go doc comment for why).
	// Later sources win on name collision.
	AgentsSrc                 []string
	HookCommand               string
	AllowedTools              []string
	EnableAllMCP              bool
	PermissionMode            string
	Effort                    string // claude reasoning-effort level for the agent session
	AnthropicAPIKey           string // used to seed customApiKeyResponses (last 20 chars)
	RepoURL, RepoBranch       string
	GitToken                  string // private-repo auth for clone + the agent's push (read from $GIT_TOKEN at runtime, never written to disk)
	GitUserName, GitUserEmail string // commit identity for the agent
	TaskBranch                string // work branch the operator opens the PR from; checked out after clone
	// CheckoutBranch is checked out (read-only, never pushed) after clone when
	// TaskBranch is empty: an MR review agent works on the PR head but the turn
	// finaliser only pushes when TaskBranch is set (issue #114 decision 4).
	CheckoutBranch string
	// FullClone, when true, skips --depth 1 so the agent gets all history and all
	// branches. Intended for project-scoped pods (brainstorm/incident/refine/
	// healthCheck) that need cross-branch context. Default false = shallow clone.
	FullClone bool
	Repos     []RepoSpec

	// Lifecycle hook commands (operator-supplied via the Project CRD, delivered
	// as HOOK_* env vars). Empty means the hook is disabled. preClone/postClone
	// are fired by Render around each clone; the conversation/turn hooks are
	// fired by the session/app layer. All run via HookRun, best-effort (RunHook).
	HookPreClone             string
	HookPostClone            string
	HookConversationStart    string
	HookConversationRestart  string
	HookAgentTurnFinished    string
	HookConversationFinished string
	// HookRun executes a hook command; production wires DefaultHookRunner. Nil
	// disables hook execution (RunHook no-ops), so Params built without it (the
	// existing bootstrap tests) keep working unchanged.
	HookRun HookRunner

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
// Returns (resumed=true, nil) on the resume path, (false, nil) on the fresh
// path, so callers can set action="resume" without a second ls-remote call.
func checkoutTaskBranch(dir, taskBranch string, fullClone bool, git GitRunner) (resumed bool, err error) {
	branchExists := git(dir, "ls-remote", "--exit-code", "--heads", "origin", taskBranch) == nil
	if branchExists {
		// Unshallow so the rebase against origin/<default> has a merge base. Only
		// when the clone was shallow (FullClone=false): `git fetch --unshallow` on
		// an already-complete repo fatals ("does not make sense"), so skip it for a
		// full clone, which already has the full history.
		if !fullClone {
			if err := git(dir, "fetch", "--unshallow", "origin"); err != nil {
				return false, fmt.Errorf("unshallow for resume of %s: %w", taskBranch, err)
			}
		}
		if err := git(dir, "fetch", "origin", taskBranch); err != nil {
			return false, fmt.Errorf("fetch remote task branch %s: %w", taskBranch, err)
		}
		return true, git(dir, "checkout", "-B", taskBranch, "FETCH_HEAD")
	}
	return false, git(dir, "checkout", "-b", taskBranch)
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
	// Branch to check out after clone: the push branch (TaskBranch) when set, else
	// a read-only CheckoutBranch (MR review on the PR head; never pushed because
	// the turn finaliser only pushes when TaskBranch is set).
	checkoutBranch := p.TaskBranch
	if checkoutBranch == "" {
		checkoutBranch = p.CheckoutBranch
	}
	if len(p.Repos) > 0 {
		if err := configureGit(p, git); err != nil {
			return err
		}
		seenDest := make(map[string]string) // dest -> URL that claimed it first
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
			// Guard: two distinct URLs with the same owner/repo on different hosts
			// map to the same dest; the second would be silently skipped (the .git
			// guard treats it as a restart of the first). Fail loudly to avoid
			// operating on the wrong repo or pushing to the wrong remote.
			if first, exists := seenDest[dest]; exists {
				return fmt.Errorf("namespace collision: repos %q and %q resolve to the same dest %s", first, r.URL, dest)
			}
			seenDest[dest] = r.URL
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				if isPrimary {
					return fmt.Errorf("mkdir parent for primary repo %s: %w", r.Name, err)
				}
				continue // non-primary parent-dir failure: skip
			}
			RunHook("preClone", p.HookPreClone, p.Workspace, []string{r.URL}, []string{"TATARA_HOOK_REPO_URL=" + r.URL}, p.HookRun, p.Log, p.M)
			cloneStart := time.Now()
			// Skip clone when the repo is already present (pod restart with persistent workspace).
			if _, statErr := os.Stat(filepath.Join(dest, ".git")); os.IsNotExist(statErr) {
				args := []string{"clone"}
				if !p.FullClone {
					args = append(args, "--depth", "1")
				}
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
			if checkoutBranch != "" {
				// Surface checkout/resume failures for ALL repos (not just the
				// primary): a secondary repo silently left on the wrong branch
				// would make the agent commit the wrong state. Fail loud so the
				// operator retries the run.
				resumed, err := checkoutTaskBranch(dest, checkoutBranch, p.FullClone, git)
				if err != nil {
					if p.M != nil {
						p.M.BootstrapCloneTotal.WithLabelValues("fail").Inc()
					}
					return fmt.Errorf("checkout task branch in %s: %w", r.Name, err)
				}
				if resumed {
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
			RunHook("postClone", p.HookPostClone, p.Workspace, []string{dest}, []string{"TATARA_HOOK_CLONE_DEST=" + dest}, p.HookRun, p.Log, p.M)
		}
	} else if p.RepoURL != "" {
		if err := configureGit(p, git); err != nil {
			return err
		}
		// Clone into a namespace subdir (workspace/owner/repo) rather than the
		// workspace root. This keeps injected session files (CLAUDE.md, .mcp.json)
		// at the workspace root outside the repo's working tree, so they are
		// never staged by CommitAndPush's `git add -A` and cannot pollute the PR.
		ns := namespacePath(p.RepoURL)
		if ns == "" || filepath.Clean(filepath.Join(p.Workspace, ns)) == filepath.Clean(p.Workspace) {
			return fmt.Errorf("cannot derive namespace path from URL %q: would clone into workspace root", p.RepoURL)
		}
		repoDest := filepath.Join(p.Workspace, ns)
		if err := os.MkdirAll(filepath.Dir(repoDest), 0o755); err != nil {
			return fmt.Errorf("mkdir parent for repo %s: %w", p.RepoURL, err)
		}
		RunHook("preClone", p.HookPreClone, p.Workspace, []string{p.RepoURL}, []string{"TATARA_HOOK_REPO_URL=" + p.RepoURL}, p.HookRun, p.Log, p.M)
		cloneStart := time.Now()
		if _, statErr := os.Stat(filepath.Join(repoDest, ".git")); os.IsNotExist(statErr) {
			args := []string{"clone"}
			if !p.FullClone {
				args = append(args, "--depth", "1")
			}
			if p.RepoBranch != "" {
				args = append(args, "--branch", p.RepoBranch)
			}
			args = append(args, p.RepoURL, repoDest)
			if err := git(p.Workspace, args...); err != nil {
				if p.M != nil {
					p.M.BootstrapCloneTotal.WithLabelValues("fail").Inc()
				}
				return fmt.Errorf("clone repo %s: %w", p.RepoURL, err)
			}
		}
		action := "clone"
		if checkoutBranch != "" {
			resumed, err := checkoutTaskBranch(repoDest, checkoutBranch, p.FullClone, git)
			if err != nil {
				if p.M != nil {
					p.M.BootstrapCloneTotal.WithLabelValues("fail").Inc()
				}
				return err
			}
			if resumed {
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
		RunHook("postClone", p.HookPostClone, p.Workspace, []string{repoDest}, []string{"TATARA_HOOK_CLONE_DEST=" + repoDest}, p.HookRun, p.Log, p.M)
	}
	renderConfigStart := time.Now()
	if err := writeIfSet(filepath.Join(p.Workspace, "CLAUDE.md"), p.ProjectClaudeMd); err != nil {
		if p.M != nil {
			p.M.BootstrapRenderTotal.WithLabelValues("fail").Inc()
		}
		return err
	}
	globalClaudeMd := strings.TrimLeft(p.GlobalClaudeMd+headlessDirective+delegationDirective, "\n")
	if err := os.WriteFile(filepath.Join(claudeHome, "CLAUDE.md"), []byte(globalClaudeMd), 0o644); err != nil {
		if p.M != nil {
			p.M.BootstrapRenderTotal.WithLabelValues("fail").Inc()
		}
		return fmt.Errorf("write global CLAUDE.md: %w", err)
	}
	if err := mergeMCP(p); err != nil {
		if p.M != nil {
			p.M.BootstrapRenderTotal.WithLabelValues("fail").Inc()
		}
		return err
	}
	if err := writeSettings(p, claudeHome); err != nil {
		if p.M != nil {
			p.M.BootstrapRenderTotal.WithLabelValues("fail").Inc()
		}
		return err
	}
	if err := writeClaudeJSON(p); err != nil {
		if p.M != nil {
			p.M.BootstrapRenderTotal.WithLabelValues("fail").Inc()
		}
		return err
	}
	// cloneSkillsRepo is fail-open (always returns nil); discard the error
	// explicitly so the intent is clear and errcheck linters are satisfied.
	_ = cloneSkillsRepo(p, git)
	if err := installSkills(p); err != nil {
		if p.M != nil {
			p.M.BootstrapRenderTotal.WithLabelValues("fail").Inc()
		}
		return err
	}
	if err := installAgents(p); err != nil {
		if p.M != nil {
			p.M.BootstrapRenderTotal.WithLabelValues("fail").Inc()
		}
		return err
	}
	if p.M != nil {
		p.M.BootstrapRenderTotal.WithLabelValues("ok").Inc()
		p.M.BootstrapDuration.Observe(time.Since(renderStart).Seconds())
	}
	if p.Log != nil {
		p.Log.Info("bootstrap config rendered", "action", "bootstrap_render",
			"duration_ms", time.Since(renderConfigStart).Milliseconds())
	}
	return nil
}

// headlessDirective is appended to the agent's global CLAUDE.md on every
// bootstrap. The agent runs in a pod with no human at the terminal, so it must
// never wait on an interactive prompt; decisions go to the issue thread instead.
const headlessDirective = `

---

## Headless agent: no interactive prompts

You run headless in a pod. There is no human at the terminal. Claude's built-in
interactive tools AskUserQuestion, ExitPlanMode and EnterPlanMode are disabled
(denied) and error if called. Do not call them. Do not enter plan mode; there is
no one to approve a plan. Do not wait on a dialog or picker.

When you need a decision, a choice between options, or any clarification,
surface it as a comment on the issue with the comment_on_issue MCP tool: lay out
the options and your recommendation there and continue with your best judgement.
If a decision blocks you from making any progress at all, call
decline_implementation with the reason. The issue thread is your only channel to
a human.
`

// delegationDirective is appended to the agent's global CLAUDE.md on every
// bootstrap (task-kind redesign Decision 6: typed-agent install path). It
// routes work to the typed subagents installed into .claude/agents/ (shipped
// by tatara-agent-skills) so planning tokens stay on the main model and
// model tiering is structural via each file's own frontmatter.
const delegationDirective = `

---

## Delegate work to typed subagents

Four typed subagents are available via the Agent tool, each pinned to a
model tier via its own frontmatter: delegate to them instead of doing this
work yourself.

- **explorer**: read-only code search, locating where something lives in
  the codebase.
- **tester**: run and write tests.
- **builder**: multi-file implementation from a clear, already-decided plan.
- **architect**: hard reasoning, design, and adversarial verification.

Keep planning, design, review, and merge decisions on yourself. Delegate the
mechanical legwork; do not delegate the thinking.
`

func writeIfSet(path, content string) error {
	if content == "" {
		return nil
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
