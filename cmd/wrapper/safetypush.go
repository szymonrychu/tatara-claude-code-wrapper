package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/bootstrap"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
)

// safetyPusher periodically pushes the task branch to origin while a turn is
// still running, so committed work is durable long before the turn finaliser
// gets to it.
//
// The gap it closes is granularity, not capability: finalizeTurn already
// commits and pushes, but only at turn end. Agent pod imp-mtg-mtg-decks-i37
// committed at 07:45 and was OOMKilled at 07:51 inside a 42-minute turn, and
// because /workspace is the container writable layer with no volume, the
// commit died with the pod having never reached the remote.
//
// PUSH ONLY. No `git add`, no `git commit`. That is the load-bearing choice:
// no index mutation means no .git/index.lock contention with the agent's own
// concurrent git calls, no chance of publishing a half-edited tree or a
// conflict marker, and a branch that only ever advances by commits the agent
// chose to make.
//
// It writes no turn record. rec.PushedRepos means "this turn produced a
// commit" and the operator consumes it (LastTurnPushedRepos drives
// handoff-note synthesis), so only finalizeTurn may set it.
type safetyPusher struct {
	workspace  string
	taskBranch string
	repos      []bootstrap.RepoSpec
	repoURL    string
	interval   time.Duration
	git        bootstrap.GitRunner
	log        *slog.Logger
	m          *metrics.Metrics

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newSafetyPusher(cfg config, git bootstrap.GitRunner, log *slog.Logger, m *metrics.Metrics) *safetyPusher {
	ctx, cancel := context.WithCancel(context.Background())
	return &safetyPusher{
		workspace:  cfg.Workspace,
		taskBranch: cfg.TaskBranch,
		repos:      cfg.Repos,
		repoURL:    cfg.RepoURL,
		interval:   time.Duration(cfg.BranchPushIntervalSeconds) * time.Second,
		git:        git,
		log:        log,
		m:          m,
		ctx:        ctx,
		cancel:     cancel,
	}
}

// Enabled reports whether the safety net should run. A Review Task checks its
// branch out read-only via CheckoutBranch and sets no TaskBranch: that branch
// is someone else's pull request head and must never be pushed. A zero
// interval is the explicit off switch.
func (s *safetyPusher) Enabled() bool { return s.taskBranch != "" && s.interval > 0 }

// Start launches the push loop. A no-op when the safety net is disabled.
func (s *safetyPusher) Start() {
	if !s.Enabled() {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.loop()
	}()
	s.log.Info("mid-turn push safety net started", "action", "safety_push",
		"branch", s.taskBranch, "interval", s.interval)
}

func (s *safetyPusher) loop() {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.pushOnce()
		}
	}
}

// Shutdown stops the loop. The turn finaliser's own commit+push is what makes
// the final state durable, so there is no last-gasp push here.
func (s *safetyPusher) Shutdown(context.Context) {
	s.cancel()
	s.wg.Wait()
}

// pushOnce pushes the task branch in every repo that has one. Every failure is
// a warning: the safety net is strictly additive, and nothing it does may
// affect the turn.
func (s *safetyPusher) pushOnce() {
	if s.taskBranch == "" {
		return
	}
	for _, dir := range s.repoDirs() {
		if err := bootstrap.PushOnly(dir, s.taskBranch, s.git); err != nil {
			s.m.SafetyPushTotal.WithLabelValues("fail").Inc()
			s.log.Warn("mid-turn safety push failed; the turn finaliser will retry at turn end",
				"action", "safety_push", "branch", s.taskBranch, "dir", dir, "error", err)
			continue
		}
		s.m.SafetyPushTotal.WithLabelValues("ok").Inc()
	}
}

// repoDirs returns the clone directory of every repo to push, in the same
// iteration shape CommitAndPushAll uses: a repo whose URL yields no namespace
// is skipped so the workspace root is never operated on.
func (s *safetyPusher) repoDirs() []string {
	var dirs []string
	if len(s.repos) > 0 {
		for _, r := range s.repos {
			if dir := bootstrap.RepoDir(s.workspace, r.URL); dir != "" {
				dirs = append(dirs, dir)
			}
		}
		return dirs
	}
	if dir := bootstrap.RepoDir(s.workspace, s.repoURL); dir != "" {
		dirs = append(dirs, dir)
	}
	return dirs
}
