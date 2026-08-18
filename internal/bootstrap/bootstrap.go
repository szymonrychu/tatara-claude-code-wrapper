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
	// ExtraSkillSources carries TATARA_EXTRA_SKILL_SOURCES: a compact JSON
	// array [{"name","url","ref","subdir"}] of Project-scoped extra skill
	// repositories, cloned and installed (per-source failure isolation) after
	// the baked skills so a project source can override a same-named baked
	// skill.
	ExtraSkillSources []byte
	SkillsSrc         []string
	SkillProfile      string // TATARA_SKILL_PROFILE; empty = install all
	SkillsRepo        string // TATARA_SKILLS_REPO; URL to clone at boot
	SkillsRef         string // TATARA_SKILLS_REF; git ref for the clone
	SkillsCloneDir    string // directory where the skills repo is cloned
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
	// BranchPushInterval is how often the wrapper's mid-turn safety net pushes
	// TaskBranch to origin (BRANCH_PUSH_INTERVAL_SECONDS). Render does not push
	// anything itself; it needs this solely to decide whether to TELL the agent
	// the safety net is running - see autoPushDirective. 0 disables the net, and
	// the directive with it.
	BranchPushInterval time.Duration
	// FullClone, when true, skips --depth 1 so the agent gets all history and all
	// branches. Intended for project-scoped pods (brainstorm/incident/refine/
	// healthCheck) that need cross-branch context. Default false = shallow clone.
	FullClone bool
	Repos     []RepoSpec

	// TaskName and ProjectName identify the work this workspace belongs to
	// (TATARA_TASK / TATARA_PROJECT). They are recorded in the workspace
	// identity file so a resumed volume can prove it holds THIS Task's tree.
	// Empty on either side means "cannot compare", and nothing is wiped.
	TaskName    string
	ProjectName string

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

// Reconcile results, reported on ccw_bootstrap_reconcile_total{result} and in
// the bootstrap_reconcile log line.
//
// The first group is the task branch against its BASE branch (issue #131). The
// second is the task branch against ITS OWN remote counterpart, which only a
// persistent workspace can ever need: without a volume the local branch is
// whatever the clone just produced, so there are no local commits to weigh
// against origin. Both groups share the one counter deliberately - they are the
// same business action ("what did bootstrap have to do to this branch") and a
// second metric would only split the query.
const (
	reconcileUpToDate       = "up_to_date"
	reconcileMerged         = "merged"
	reconcileConflict       = "conflict"
	reconcileFetchFail      = "fetch_fail"
	reconcileBaseUnresolved = "base_unresolved"

	// localOnly: the branch exists on this volume and nowhere else. It is
	// checked out as-is and published, because one lost volume would otherwise
	// lose every commit on it.
	reconcileLocalOnly = "local_only"
	// localAhead: local already contains the remote tip; nothing to merge.
	reconcileLocalAhead = "local_ahead"
	// remoteAhead: local is contained in the remote tip, so the remote can be
	// taken with a fast-forward that adds commits and discards none.
	reconcileRemoteAhead = "remote_ahead"
	// localRemoteMerged / localRemoteConflict: genuine divergence, merged or
	// aborted. Never a reset: local commits are authoritative and force-push is
	// denied, so a reset would strand the branch permanently.
	reconcileLocalRemoteMerged   = "local_remote_merged"
	reconcileLocalRemoteConflict = "local_remote_conflict"
)

// checkoutTaskBranch checks out taskBranch in dir and reconciles it, first
// against its own remote counterpart and then - when the agent OWNS the branch
// (reconcile=true, i.e. TaskBranch is set) - against the repo's base branch.
// Returns (resumed, <local-vs-remote result>, <base result>, err); resumed=true
// means the branch was inherited rather than created, so callers can set
// action="resume" and report both outcomes without a second probe.
//
// persistent says the checkout survived from an earlier pod (a mounted volume,
// not a clone this Render just made). It is the ONLY reason the local half of
// the probe below can be true, and it is passed rather than inferred so a fresh
// clone keeps its exact previous behaviour: a just-cloned repo has no
// refs/heads/<taskBranch> to find, and probing for one would only add a git
// call whose answer is known.
//
// The four arms, and why each is what it is. The rule they all serve: local
// commits are authoritative and are never discarded, the remote is
// authoritative for nothing, and every operation is additive - merge, commit,
// or merge --abort - because those are the only ones that cannot lose a side.
// Force-push is denied by the settings deny list, so a branch whose history was
// rewritten could never be published again; the merge is the only recoverable
// option, not a stylistic preference.
//
//	local no,  remote no  -> `checkout -b`: a genuinely fresh task branch.
//	local yes, remote no  -> plain `checkout`, then PUBLISH it. This is the
//	    OOMKill case: the pod committed and died before anything reached
//	    origin. `checkout -b` here is fatal ("a branch named X already
//	    exists"), and with a persistent volume that fatality is permanent -
//	    the pod crash-loops to the residency cap and no turn ever runs. The
//	    push matters as much as the checkout: those commits exist on exactly
//	    one disk until it happens.
//	local no,  remote yes -> unchanged: unshallow, fetch, `checkout -B
//	    FETCH_HEAD`. A reset is safe here precisely because there is nothing
//	    local to lose.
//	local yes, remote yes -> NEVER -B and never a reset, which would
//	    hard-reset the branch onto the remote tip and destroy exactly the
//	    unpushed commits the volume exists to preserve. Contained either way
//	    is a no-op or a fast-forward; genuine divergence is merged, and a
//	    conflicting merge is aborted and handed to the agent as a first job
//	    rather than failing the boot (the same inversion issue #131 made for
//	    the base merge).
func checkoutTaskBranch(dir, taskBranch, baseBranch string, fullClone, reconcile, persistent bool, git GitRunner) (resumed bool, localResult, baseResult string, err error) {
	localExists := persistent && git(dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+taskBranch) == nil
	remoteExists := git(dir, "ls-remote", "--exit-code", "--heads", "origin", taskBranch) == nil

	switch {
	case !localExists && !remoteExists:
		return false, "", "", git(dir, "checkout", "-b", taskBranch)

	case localExists && !remoteExists:
		if err := git(dir, "checkout", taskBranch); err != nil {
			return false, "", "", fmt.Errorf("checkout local task branch %s: %w", taskBranch, err)
		}
		localResult = reconcileLocalOnly
		unshallowBestEffort(dir, fullClone, git)
		if reconcile {
			// Best-effort: an unreachable origin must not fail a boot whose
			// checkout already succeeded, and the mid-turn safety net (PushOnly)
			// retries every couple of minutes for the rest of the turn.
			_ = git(dir, "push", "--no-verify", "-u", "origin", taskBranch)
		}

	case !localExists && remoteExists:
		// Unshallow so the merge against origin/<base> has a merge base. Only
		// when the clone was shallow (FullClone=false): `git fetch --unshallow` on
		// an already-complete repo fatals ("does not make sense"), so skip it for a
		// full clone, which already has the full history.
		if !fullClone {
			if err := git(dir, "fetch", "--unshallow", "origin"); err != nil {
				return false, "", "", fmt.Errorf("unshallow for resume of %s: %w", taskBranch, err)
			}
		}
		if err := git(dir, "fetch", "origin", taskBranch); err != nil {
			return false, "", "", fmt.Errorf("fetch remote task branch %s: %w", taskBranch, err)
		}
		if err := git(dir, "checkout", "-B", taskBranch, "FETCH_HEAD"); err != nil {
			return false, "", "", err
		}

	default:
		if err := git(dir, "checkout", taskBranch); err != nil {
			return false, "", "", fmt.Errorf("checkout local task branch %s: %w", taskBranch, err)
		}
		unshallowBestEffort(dir, fullClone, git)
		if err := git(dir, "fetch", "origin", taskBranch); err != nil {
			return false, "", "", fmt.Errorf("fetch remote task branch %s: %w", taskBranch, err)
		}
		localResult = mergeRemoteTaskBranch(dir, taskBranch, git)
	}

	if !reconcile {
		return true, localResult, "", nil
	}
	baseResult, err = reconcileWithBase(dir, taskBranch, baseBranch, git)
	return true, localResult, baseResult, err
}

// unshallowBestEffort completes a shallow clone so later merges have a merge
// base. Unlike the fresh-clone resume path this one CANNOT treat a failure as
// fatal: a persistent workspace may already carry the full history from an
// earlier boot, and `git fetch --unshallow` on a complete repo fatals with
// "does not make sense". The runner returns only an exit code (git's output is
// dropped deliberately), so the two cases are indistinguishable here - and the
// fetch that follows is the one whose failure is checked.
func unshallowBestEffort(dir string, fullClone bool, git GitRunner) {
	if fullClone {
		return
	}
	_ = git(dir, "fetch", "--unshallow", "origin")
}

// mergeRemoteTaskBranch reconciles a task branch that exists BOTH locally and
// on origin, having already fetched the remote tip into FETCH_HEAD. It never
// resets: precedence is local commits over remote commits, and the only way to
// honour that while still taking the remote's work is a merge.
//
// A conflict is not an error, for the same reason it is not one in
// reconcileWithBase: it is exactly the work an agent exists to do, and failing
// the boot instead turns one conflict into hundreds of dead pods. The merge is
// aborted so the turn finaliser's `git add -A` can never publish conflict
// markers, and the conflict travels out as a result the directive turns into
// the agent's first job.
func mergeRemoteTaskBranch(dir, taskBranch string, git GitRunner) string {
	if git(dir, "merge-base", "--is-ancestor", "FETCH_HEAD", "HEAD") == nil {
		return reconcileLocalAhead
	}
	if git(dir, "merge-base", "--is-ancestor", "HEAD", "FETCH_HEAD") == nil {
		if err := git(dir, "merge", "--ff-only", "FETCH_HEAD"); err != nil {
			return reconcileLocalRemoteConflict
		}
		return reconcileRemoteAhead
	}
	msg := "tatara bootstrap: reconcile local and remote " + taskBranch
	if err := git(dir, "merge", "-m", msg, "FETCH_HEAD"); err != nil {
		_ = git(dir, "merge", "--abort")
		return reconcileLocalRemoteConflict
	}
	return reconcileLocalRemoteMerged
}

// reconcileWithBase brings a resumed task branch up to date with the repo's
// base branch by MERGING origin/<base> into it (issue #131: a resumed branch
// was silently left days behind main, so the agent reasoned about code that no
// longer existed).
//
// Merge, never rebase or reset. The task branch is already published on origin
// and this platform forbids force-push, so rewriting its history would leave
// the agent's next plain `git push` rejected as non-fast-forward: its committed
// work would be stranded on a branch nobody can advance, and recovering would
// need exactly the force-push that is denied. A merge only ever ADDS a commit,
// so every commit the agent has already made stays reachable and the existing
// non-force push in CommitAndPush stays a fast-forward. It is also the only
// strategy with a safe failure mode: `git merge --abort` restores HEAD, the
// index and the working tree exactly, whereas a failed rebase leaves the repo
// mid-replay with detached HEAD.
//
// The reconcile is skipped entirely when the branch already contains the base
// tip, so a healthy resume does not accrue an empty merge commit per turn.
//
// A genuine conflict is NOT an error. It used to be, and that made every
// conflicting task branch a permanent boot failure: Render failed, the wrapper
// exited non-zero, the pod never became Ready, and the operator respawned it
// every few minutes until the 24h residency cap - hundreds of pods, not one
// turn. A conflict is exactly the kind of work an agent exists to do, so the
// result is reported (counted, logged at ERROR) and handed to the agent as a
// mandatory first job via the injected CLAUDE.md; see resumeDirective.
//
// The merge is still aborted. CommitAndPush stages the whole tree with
// `git add -A` and commits with --no-verify, so leaving the conflicted tree in
// place would publish `<<<<<<<` markers to the branch on the first turn that
// ends. Aborting keeps the worst case "the agent did nothing" instead of "the
// agent pushed conflict markers".
//
// A failed fetch stays fatal: unlike a conflict it leaves us with no view of
// the remote at all, so there is no truthful state to describe to the agent.
func reconcileWithBase(dir, taskBranch, baseBranch string, git GitRunner) (string, error) {
	if err := git(dir, "fetch", "origin"); err != nil {
		return reconcileFetchFail, fmt.Errorf("fetch origin to reconcile resumed branch %s: %w", taskBranch, err)
	}
	baseRef := "origin/" + baseBranch
	if baseBranch == "" {
		// No base branch pinned by the operator: resolve the remote's own
		// default head. If that fails we cannot know what "current" means, so
		// report it (WARN + counted result) rather than pretend the tree is fresh.
		if err := git(dir, "remote", "set-head", "origin", "--auto"); err != nil {
			return reconcileBaseUnresolved, nil
		}
		baseRef = "origin/HEAD"
	}
	// Exit 0 means baseRef is already an ancestor of HEAD: nothing to merge.
	if git(dir, "merge-base", "--is-ancestor", baseRef, "HEAD") == nil {
		return reconcileUpToDate, nil
	}
	msg := "tatara bootstrap: reconcile " + taskBranch + " with " + baseRef
	if err := git(dir, "merge", "-m", msg, baseRef); err != nil {
		// Abort restores the pre-merge HEAD, index and working tree; no commit
		// the agent made is lost, and no conflict marker can be committed by
		// the turn finaliser. The conflict then travels out as a result with a
		// NIL error, and becomes the agent's first job instead of a boot failure.
		_ = git(dir, "merge", "--abort")
		return reconcileConflict, nil
	}
	return reconcileMerged, nil
}

// recordReconcile emits the reconcile outcome as a counted, logged business
// action (rules 12+13). A stale resume that got merged is as important to see
// as one that failed, so every non-empty result is reported.
func recordReconcile(p Params, repo, taskBranch, baseRef, result string) {
	if result == "" {
		return
	}
	if p.M != nil {
		p.M.BootstrapReconcileTotal.WithLabelValues(result).Inc()
	}
	if p.Log == nil {
		return
	}
	args := []any{"action", "bootstrap_reconcile", "repo", repo, "task_branch", taskBranch,
		"base_ref", baseRef, "result", result}
	switch result {
	case reconcileConflict, reconcileFetchFail, reconcileLocalRemoteConflict:
		p.Log.Error("resumed task branch could not be reconciled with its base branch", args...)
	case reconcileBaseUnresolved:
		p.Log.Warn("resumed task branch not reconciled: base branch could not be resolved", args...)
	default:
		p.Log.Info("resumed task branch reconciled with its base branch", args...)
	}
}

// baseRefName is the ref recordReconcile reports for a repo whose base branch
// is baseBranch, mirroring reconcileWithBase's own resolution.
func baseRefName(baseBranch string) string {
	if baseBranch == "" {
		return "origin/HEAD"
	}
	return "origin/" + baseBranch
}

// resumedRepo records one repo whose task branch already existed on origin and
// was resumed rather than created. Collected during the clone loop so the
// post-loop config render can tell the agent what it inherited, and - when the
// base merge could not be completed - that finishing it is its first job.
type resumedRepo struct {
	name       string
	dir        string
	taskBranch string
	baseRef    string
	// result is the reconcile against the repo's BASE branch.
	result string
	// localResult is the reconcile of the task branch against its own remote
	// counterpart, which only a persistent workspace can produce.
	localResult string
	// notes are the agent-visible repairs PrepareWorkspace made to this
	// checkout before anything was checked out (an aborted operation, a rescue
	// commit, a reattached HEAD). Platform noise - stale locks, a half-cloned
	// .git - is deliberately absent: it is counted and logged, never surfaced.
	notes []string
	// required are repairs the agent must act on before anything else, rendered
	// under the mandatory heading alongside the unfinished merges.
	required []string
}

// reconciledCleanly reports whether either reconcile did visible, successful
// work on this branch - the tier-one, informational case.
func (r resumedRepo) reconciledCleanly() bool {
	switch r.result {
	case reconcileMerged, reconcileUpToDate:
		return true
	}
	switch r.localResult {
	case reconcileLocalOnly, reconcileLocalAhead, reconcileRemoteAhead, reconcileLocalRemoteMerged:
		return true
	}
	return false
}

// resumeDirective renders the resumed-branch block appended to the agent's
// global CLAUDE.md, in three tiers.
//
// Tier one (merged / up to date / any clean local-vs-remote outcome) is
// informational: the branch is not empty and the code the agent is about to
// read includes a previous session's work.
//
// The middle tier reports what bootstrap had to REPAIR on a workspace volume
// that outlived its pod - an aborted merge or rebase, uncommitted work rescued
// into a commit, a reattached HEAD. It sits between the two because it is
// neither pure background nor an instruction: nothing is required of the agent,
// but the branch is not shaped the way it left it. Platform noise (stale lock
// files, a half-cloned .git, a wrong-Task wipe) never reaches this tier - it is
// counted and logged, because telling the agent about it would spend prompt
// budget on something it cannot act on.
//
// Tier two (conflict / base unresolved) is a hard requirement. Bootstrap could
// not complete a merge and had to abort, so the tree the agent is about to read
// is stale by exactly the changes that conflicted. base unresolved joins
// conflict rather than tier one because it is the same epistemic state - we
// could not prove the branch is current - and the same remedy applies; tier one
// is reserved for branches we KNOW carry the tip.
//
// Inside tier two the LOCAL-versus-remote merge is always listed before the
// base merge. That order is the point of it: the base merge is only meaningful
// once the branch actually contains everything both sides of the task branch
// committed, so resolving them the other way round means merging the base into
// a branch that is still missing half its own work.
//
// Repos with no result and no notes are omitted: that is the read-only review
// checkout path, where the branch belongs to someone else's pull request and
// the agent must neither reconcile nor push it.
func resumeDirective(repos []resumedRepo, workspaceNotes []string) string {
	var clean, recovered, mustInspect, localUnreconciled, baseUnreconciled []resumedRepo
	for _, r := range repos {
		if len(r.required) > 0 {
			mustInspect = append(mustInspect, r)
		}
		if r.localResult == reconcileLocalRemoteConflict {
			localUnreconciled = append(localUnreconciled, r)
		}
		switch r.result {
		case reconcileConflict, reconcileBaseUnresolved:
			baseUnreconciled = append(baseUnreconciled, r)
		}
		if len(r.notes) > 0 {
			recovered = append(recovered, r)
		}
		if r.reconciledCleanly() {
			clean = append(clean, r)
		}
	}
	listed := orderedRepoUnion(mustInspect, localUnreconciled, baseUnreconciled, recovered, clean)
	if len(listed) == 0 && len(workspaceNotes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(resumeHeader)
	for _, r := range listed {
		fmt.Fprintf(&b, "- `%s` at `%s`: branch `%s`, base ref `%s`.\n", r.name, r.dir, r.taskBranch, r.baseRef)
	}
	if len(recovered) > 0 || len(workspaceNotes) > 0 {
		b.WriteString(resumeRecoveredHeader)
		for _, n := range workspaceNotes {
			fmt.Fprintf(&b, "- %s\n", n)
		}
		for _, r := range recovered {
			for _, n := range r.notes {
				fmt.Fprintf(&b, "- In `%s`: %s\n", r.dir, n)
			}
		}
	}
	if len(mustInspect) > 0 || len(localUnreconciled) > 0 || len(baseUnreconciled) > 0 {
		b.WriteString(resumeConflictRequirement)
		// A tree bootstrap could not make sense of comes first: whatever it holds
		// may change what the merges below are even supposed to produce.
		for _, r := range mustInspect {
			for _, n := range r.required {
				fmt.Fprintf(&b, "- In `%s`: %s.\n", r.dir, n)
			}
		}
		for _, r := range localUnreconciled {
			fmt.Fprintf(&b, "- In `%s`: fetch origin, merge `origin/%s` into your local `%s`, resolve every conflict, commit the merge.\n",
				r.dir, r.taskBranch, r.taskBranch)
		}
		for _, r := range baseUnreconciled {
			fmt.Fprintf(&b, "- In `%s`: fetch origin, merge `%s` into `%s`, resolve every conflict, commit the merge.\n",
				r.dir, r.baseRef, r.taskBranch)
		}
		b.WriteString(resumeConflictClosing)
	}
	return b.String()
}

// orderedRepoUnion concatenates the buckets in priority order, keeping the
// first occurrence of each repo directory. A repo can land in several buckets
// at once (a local conflict on a branch whose base merged cleanly is the
// ordinary case), and the header must name it once.
func orderedRepoUnion(buckets ...[]resumedRepo) []resumedRepo {
	seen := map[string]bool{}
	var out []resumedRepo
	for _, bucket := range buckets {
		for _, r := range bucket {
			if seen[r.dir] {
				continue
			}
			seen[r.dir] = true
			out = append(out, r)
		}
	}
	return out
}

// resumeHeader / resumeConflictRequirement / resumeConflictClosing are the
// static prose of the resumed-branch block. They name no MCP tool, and must
// keep naming none: promptguard treats every snake-case token inside a prompt
// literal as a tool name and fails the build when it is not advertised by the
// live server. Write "task branch", never the underscored form.
const resumeHeader = `

---

## Resumed task branch

You are not starting from an empty branch. In the repositories below the task
branch already existed - on origin, on this workspace volume, or both - and
carries commits from an earlier session of this same task. It is checked out for
you, so the work you find there is your own prior work; read it before you plan,
and continue it rather than restarting it.

`

const resumeRecoveredHeader = `
### Workspace recovered from an interrupted session

This workspace sits on a volume that outlived the pod using it, and the previous
session was cut off rather than finished. Bootstrap repaired what it found
before handing you the tree. Nothing was thrown away, but the branch is not
shaped the way the last session left it, so read these before you plan:

`

const resumeConflictRequirement = `
### Hard requirement: repair the interrupted session first

The previous session was cut off part way through something bootstrap could not
finish for you. Nothing you committed before is lost and the working tree is
clean at the tip of the task branch - but the branch is missing exactly the
changes that could not be applied, and the code you are about to read is stale
by them.

Resolving this is a hard requirement and your first action. Before you read or
write any other code, before any planning, work through the list below IN THE
ORDER GIVEN, which is the order in which the items depend on each other: a tree
bootstrap could not make sense of is inspected first, because what it holds can
change what the merges are supposed to produce; then the merge of your own
branch against origin, because merging the base branch into a branch that is
still missing half its own commits reconciles nothing; then the base branch.

`

const resumeConflictClosing = `
Do not begin any other work until every merge above is committed. Do not
resolve a conflict by blindly taking one side; read both and keep the intent of
each. If a merge genuinely cannot be resolved, say so in the outcome you submit
rather than continuing on a stale tree.
`

func Render(p Params, git GitRunner) error {
	renderStart := time.Now()
	claudeHome := filepath.Join(p.HomeDir, ".claude")
	if err := os.MkdirAll(claudeHome, 0o755); err != nil {
		return fmt.Errorf("mkdir claude home: %w", err)
	}
	if err := os.MkdirAll(p.Workspace, 0o755); err != nil {
		return fmt.Errorf("mkdir workspace: %w", err)
	}
	// Whose workspace is this? A per-Task volume can be rebound to another Task,
	// and every phase below would otherwise treat that Task's tree as our own.
	workspaceNotes, err := claimWorkspace(p, p.Log, p.M)
	if err != nil {
		return err
	}
	// Branch to check out after clone: the push branch (TaskBranch) when set, else
	// a read-only CheckoutBranch (MR review on the PR head; never pushed because
	// the turn finaliser only pushes when TaskBranch is set).
	checkoutBranch := p.TaskBranch
	if checkoutBranch == "" {
		checkoutBranch = p.CheckoutBranch
	}
	// Every repo whose branch was RESUMED, carried out of the clone loop so the
	// post-loop CLAUDE.md render can state it to the agent (see resumeDirective).
	var resumedRepos []resumedRepo
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
			prep, prepErr := PrepareWorkspace(dest, git, p.Log, p.M)
			if prepErr != nil {
				if isPrimary {
					return fmt.Errorf("prepare workspace for primary repo %s: %w", r.Name, prepErr)
				}
				continue // non-primary: same isolation the clone failure below uses
			}
			// Skip clone when the repo is already present (pod restart with a
			// persistent workspace). persistent is what tells checkoutTaskBranch a
			// local task branch is even possible - see its doc comment.
			persistent := true
			if _, statErr := os.Stat(filepath.Join(dest, ".git")); os.IsNotExist(statErr) {
				persistent = false
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
				resumed, localResult, result, err := checkoutTaskBranch(dest, checkoutBranch, r.Branch, p.FullClone, p.TaskBranch != "", persistent, git)
				recordReconcile(p, r.Name, checkoutBranch, baseRefName(r.Branch), localResult)
				recordReconcile(p, r.Name, checkoutBranch, baseRefName(r.Branch), result)
				if err != nil {
					if p.M != nil {
						p.M.BootstrapCloneTotal.WithLabelValues("fail").Inc()
					}
					return fmt.Errorf("checkout task branch in %s: %w", r.Name, err)
				}
				if resumed || len(prep.Notes) > 0 || len(prep.Required) > 0 {
					action = "resume"
					resumedRepos = append(resumedRepos, resumedRepo{
						name: r.Name, dir: dest, taskBranch: checkoutBranch,
						baseRef: baseRefName(r.Branch), result: result,
						localResult: localResult, notes: prep.Notes, required: prep.Required,
					})
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
		prep, prepErr := PrepareWorkspace(repoDest, git, p.Log, p.M)
		if prepErr != nil {
			return fmt.Errorf("prepare workspace for repo %s: %w", p.RepoURL, prepErr)
		}
		persistent := true
		if _, statErr := os.Stat(filepath.Join(repoDest, ".git")); os.IsNotExist(statErr) {
			persistent = false
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
			resumed, localResult, result, err := checkoutTaskBranch(repoDest, checkoutBranch, p.RepoBranch, p.FullClone, p.TaskBranch != "", persistent, git)
			recordReconcile(p, p.RepoURL, checkoutBranch, baseRefName(p.RepoBranch), localResult)
			recordReconcile(p, p.RepoURL, checkoutBranch, baseRefName(p.RepoBranch), result)
			if err != nil {
				if p.M != nil {
					p.M.BootstrapCloneTotal.WithLabelValues("fail").Inc()
				}
				return err
			}
			if resumed || len(prep.Notes) > 0 || len(prep.Required) > 0 {
				action = "resume"
				resumedRepos = append(resumedRepos, resumedRepo{
					name: p.RepoURL, dir: repoDest, taskBranch: checkoutBranch,
					baseRef: baseRefName(p.RepoBranch), result: result,
					localResult: localResult, notes: prep.Notes, required: prep.Required,
				})
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
	// The resumed-branch block goes on the GLOBAL CLAUDE.md, alongside the
	// headless and delegation directives, not on the workspace one: this repo
	// already treats the global file as the agent's highest-priority
	// instruction surface, it is written unconditionally (the workspace
	// CLAUDE.md write is skipped entirely when the Project ships no
	// ProjectClaudeMd, which would silently drop the requirement), and it stays
	// in scope no matter which repo subdirectory the agent works from.
	globalClaudeMd := strings.TrimLeft(p.GlobalClaudeMd+headlessDirective+delegationDirective+
		autoPushDirective(p.TaskBranch, p.BranchPushInterval)+resumeDirective(resumedRepos, workspaceNotes), "\n")
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
	// explicitly so the intent is clear and errcheck linters are satisfied. Its
	// return is not a health signal either way - a skipped clone over a
	// half-populated dir also returns nil - so the check below keys on the
	// installed count instead.
	_ = cloneSkillsRepo(p, git)
	installedSkills, err := installSkills(p)
	if err != nil {
		if p.M != nil {
			p.M.BootstrapRenderTotal.WithLabelValues("fail").Inc()
		}
		return err
	}
	// A boot that configured skill sources and installed nothing produces a
	// DIFFERENT agent, not a degraded one: no council harness, no guardrails, no
	// TDD skill, no typed-subagent palette - while the operator's job text still
	// names the skills the turn must invoke. Nothing is baked into the image, so
	// the clone is the only source and there is no floor to fall back to. Fail
	// loudly here rather than let the turn run and submit a wrong outcome.
	// Configuring no SkillsSrc at all remains a deliberate skill-less boot.
	if installedSkills == 0 && hasSkillsSrc(p) {
		if p.M != nil {
			p.M.BootstrapRenderTotal.WithLabelValues("fail").Inc()
		}
		return fmt.Errorf("no skills installed from %v: the skills clone is missing or empty", p.SkillsSrc)
	}
	// Extra project skill sources (fail-open, per-source isolation): installed
	// after the baked skills so a project source can override a same-named
	// baked skill. Never errors Render.
	_ = installExtraSkillSources(p, git)
	if err := installAgents(p); err != nil {
		if p.M != nil {
			p.M.BootstrapRenderTotal.WithLabelValues("fail").Inc()
		}
		return err
	}
	// Stamped last, and only on success: an identity written before the clones
	// completed would let the next boot read a half-built tree as proof this
	// workspace is already this Task's own.
	if err := writeWorkspaceIdentity(p); err != nil {
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
//
// Every MCP tool name in this constant is checked against the baked cli's live
// tools/list by TestAgentPromptToolNamesAreAdvertised, which the Dockerfile's
// guard stage runs on every image build. It named comment_on_issue and
// decline_implementation until 2026-08-08, 14 days after tatara-cli reaped both
// (#136): a blocked agent got `tool not found` from the highest-priority
// instruction surface it has, at the moment it was already stuck. Do not add a
// tool name here without confirming the live server advertises it.
const headlessDirective = `

---

## Headless agent: no interactive prompts

You run headless in a pod. There is no human at the terminal. Claude's built-in
interactive tools AskUserQuestion, ExitPlanMode and EnterPlanMode are disabled
(denied) and error if called. Do not call them. Do not enter plan mode; there is
no one to approve a plan. Do not wait on a dialog or picker.

When you need a decision, a choice between options, or any clarification,
surface it on the issue thread with the issue_write(action="comment", ...) MCP
tool: lay out the options and your recommendation there and continue with your
best judgement.

If a decision blocks you from making any progress at all, say so in the outcome
you submit: end the turn through submit_outcome with the reason spelled out.
Every task ends through submit_outcome, and it is the one channel to a human
your agent kind is always granted - issue_write is not granted to every kind,
and your skill names the tools you actually have.
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

// autoPushDirective returns the mid-turn-push block for the agent's global
// CLAUDE.md, or "" when this pod does not actually auto-push.
//
// It gates on AutoPushEnabled - the SAME predicate safetyPusher.Enabled uses -
// and must keep doing so rather than re-deriving the condition. Both ways of
// disabling the net reach the same lie if the two drift: a read-only review
// checkout (no TaskBranch) and BRANCH_PUSH_INTERVAL_SECONDS=0 both leave
// nothing pushing, and an agent told otherwise will skip an amend it was free
// to make.
//
// Like the resume block, this names no MCP tool and must keep naming none:
// promptguard treats every snake-case token inside a prompt literal as a tool
// name and fails the build when the live server does not advertise it. Write
// "task branch", never the underscored form.
func autoPushDirective(taskBranch string, interval time.Duration) string {
	if !AutoPushEnabled(taskBranch, interval) {
		return ""
	}
	return autoPushBlock
}

const autoPushBlock = `

---

## Your task branch is pushed for you, mid-turn

The wrapper pushes your task branch to origin every couple of minutes, not only
when the turn ends. It is a safety net: this pod's disk does not survive the
pod, so a commit that has not reached origin is lost if the pod is killed. The
wrapper only ever pushes - it never stages and never commits on your behalf, so
what reaches origin is exactly the commits you chose to make.

The consequence for you is that a commit is public the moment you make it. Do
not plan to amend, reword, squash, or otherwise rewrite a commit once it
exists: publishing a rewritten commit needs a force-push, and force-push is
denied in this pod. If you need to change something you have already committed,
make a new commit on top of it.

Commit when your work reaches a sensible point, and commit more often than you
otherwise would. Every commit you make is durable; everything you have not
committed is not.
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
