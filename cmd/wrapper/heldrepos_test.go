package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/obs"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/webhook"
)

func TestMergeRepoLists(t *testing.T) {
	tests := []struct {
		name                   string
		heldPushed, heldFailed []string
		thisPushed, thisFailed []string
		wantPushed, wantFailed []string
	}{
		{
			name: "nothing held and nothing pushed stays nil",
		},
		{
			name:       "nothing held is the identity",
			thisPushed: []string{"a"}, thisFailed: []string{"b"},
			wantPushed: []string{"a"}, wantFailed: []string{"b"},
		},
		{
			name:       "nothing this turn returns the held set",
			heldPushed: []string{"a", "b"}, heldFailed: []string{"c"},
			wantPushed: []string{"a", "b"}, wantFailed: []string{"c"},
		},
		{
			name:       "held first then this turn, deduped",
			heldPushed: []string{"a", "b"}, thisPushed: []string{"b", "c"},
			wantPushed: []string{"a", "b", "c"},
		},
		{
			// B1: a held failure is dropped the moment any turn in the chain
			// pushes that repo. A stale failedRepos is an active instruction to
			// redo work that has since landed.
			name:       "a later push cancels a held failure",
			heldPushed: []string{"a"}, heldFailed: []string{"c"},
			thisPushed: []string{"c"},
			wantPushed: []string{"a", "c"},
		},
		{
			// The held failure survives when nothing pushes it.
			name:       "a held failure with no later push survives",
			heldFailed: []string{"c"},
			thisPushed: []string{"a"},
			wantPushed: []string{"a"}, wantFailed: []string{"c"},
		},
		{
			// B1': the extra subtraction. A repo held as pushed whose push
			// fails this turn must leave the pushed list, or the two lists
			// overlap and the operator renders both sentences for one repo.
			name:       "this turn's failure wins over a held push",
			heldPushed: []string{"a", "b"},
			thisFailed: []string{"a"},
			wantPushed: []string{"b"}, wantFailed: []string{"a"},
		},
		{
			name:       "the lists are always disjoint",
			heldPushed: []string{"a"}, heldFailed: []string{"a"},
			thisPushed: []string{"a"}, thisFailed: []string{"a"},
			wantFailed: []string{"a"},
		},
		{
			// omitempty on the wire: an empty result must be nil, not [].
			name:       "a fully cancelled list is nil, not empty",
			heldFailed: []string{"c"},
			thisPushed: []string{"c"},
			wantPushed: []string{"c"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pushed, failed := mergeRepoLists(tc.heldPushed, tc.heldFailed, tc.thisPushed, tc.thisFailed)
			require.Equal(t, tc.wantPushed, pushed)
			require.Equal(t, tc.wantFailed, failed)
			if len(tc.wantPushed) == 0 {
				require.Nil(t, pushed, "an empty result must be nil so omitempty drops the key")
			}
			if len(tc.wantFailed) == 0 {
				require.Nil(t, failed, "an empty result must be nil so omitempty drops the key")
			}
		})
	}
}

// rejectedOutcomeTranscript writes a transcript whose submit_outcome call was
// rejected by the operator, which is what arms the outcome-reprompt branch.
func rejectedOutcomeTranscript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	lines := []string{
		`{"type":"assistant","uuid":"u2","sessionId":"s1","timestamp":"2026-08-18T06:25:57.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu2","name":"mcp__tatara__submit_outcome","input":{"action":"submitted"}}]}}`,
		`{"type":"user","uuid":"u3","sessionId":"s1","timestamp":"2026-08-18T06:25:58.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu2","is_error":true,"content":"400: {\"error\":\"action=submitted but this task owns no open MR\"}"}]}}`,
	}
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
	return path
}

type repoListHarness struct {
	app       *app
	mgr       *session.Manager
	sender    *webhook.Sender
	url       string
	delivered chan map[string]any
	logs      *bytes.Buffer
}

// newRepoListHarness drives finalizeTurn directly with TaskBranch unset, so the
// commit/push block is skipped and rec.PushedRepos/rec.FailedRepos are the ones
// the test sets - byte-identically to what app.go:270-271 writes on the
// multi-repo path. gitRunner() is package-level with no seam into finalizeTurn,
// and CommitAndPush's clean-index short-circuit is already covered by fakes in
// internal/bootstrap; faking git here would re-prove that dependency instead of
// isolating this defect.
func newRepoListHarness(t *testing.T) *repoListHarness {
	t.Helper()
	h := &repoListHarness{
		delivered: make(chan map[string]any, 4),
		logs:      &bytes.Buffer{},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.delivered <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	h.url = srv.URL

	// No tailer: this test is about the repo lists, and DrainInternalIssues
	// short-circuits to nil without one rather than burning its 2s catch-up
	// timeout waiting for a tailer that is not following anything.
	h.mgr = session.New(session.Config{}, turn.NewStore(), metrics.New(prometheus.NewRegistry()),
		testLogger(), time.Now, func() string { return "turn-1" })
	h.sender = webhook.New(webhook.Config{Retries: 1}, metrics.New(prometheus.NewRegistry()), testLogger())
	h.app = &app{
		log:  testLogger(),
		sess: h.mgr,
		submitFn: func(text, cb string) (string, error) {
			return "turn-next", nil
		},
	}
	return h
}

// suppressed runs a turn whose submit_outcome was rejected: the re-prompt fires
// and the callback is suppressed.
func (h *repoListHarness) suppressed(t *testing.T, id string, pushed, failed []string) {
	t.Helper()
	h.mgr.SetTranscriptPathForTest(rejectedOutcomeTranscript(t))
	rec := &turn.Record{ID: id, CallbackURL: h.url, PushedRepos: pushed, FailedRepos: failed}
	h.app.finalizeTurn(rec, config{}, metrics.New(prometheus.NewRegistry()),
		obs.NewLogger(h.logs, slog.LevelInfo), h.sender, "")
	select {
	case <-h.delivered:
		t.Fatalf("turn %s's callback must be suppressed while the agent is re-prompted", id)
	case <-time.After(200 * time.Millisecond):
	}
}

// clean runs a turn with no rejected outcome: its callback is delivered.
func (h *repoListHarness) clean(t *testing.T, id string, pushed, failed []string) map[string]any {
	t.Helper()
	h.mgr.SetTranscriptPathForTest("")
	rec := &turn.Record{ID: id, CallbackURL: h.url, PushedRepos: pushed, FailedRepos: failed}
	h.app.finalizeTurn(rec, config{}, metrics.New(prometheus.NewRegistry()),
		obs.NewLogger(h.logs, slog.LevelInfo), h.sender, "")
	select {
	case body := <-h.delivered:
		return body
	case <-time.After(2 * time.Second):
		t.Fatalf("turn %s's callback never delivered", id)
		return nil
	}
}

// A HOLD MUST NEVER CLOBBER ANOTHER GOROUTINE'S HELD SET.
//
// finalizeTurn runs on a per-turn goroutine and the take/hold pair is split
// across it: only each half is atomic, not the pair. Two finalisers overlap
// whenever one is still inside CommitAndPushAll while the next turn's
// re-prompt has already been submitted. If the hold ASSIGNED, a finaliser
// whose own lists are both empty would write nil over the set the other one
// just parked - losing exactly the payload this whole change exists to save.
//
// holdInternalIssues survives the same split because it early-returns on empty
// AND appends. The hold folds instead, which is the identity on the sequential
// path (takePendingRepoLists runs unconditionally first, so the held fields are
// always nil by the time a hold can run) and keeps the subtraction rules under
// the race.
func TestHoldRepoLists_DoesNotClobberAnEarlierHold(t *testing.T) {
	a := &app{log: testLogger()}

	a.holdRepoLists([]string{"repo-a"}, []string{"repo-c"})
	// A second finaliser reaches its hold with nothing of its own.
	a.holdRepoLists(nil, nil)

	pushed, failed := a.takePendingRepoLists()
	require.Equal(t, []string{"repo-a"}, pushed, "an empty hold must not erase a parked set")
	require.Equal(t, []string{"repo-c"}, failed, "an empty hold must not erase a parked set")

	// And the fold still applies the subtraction: a later push cancels the
	// held failure rather than stacking beside it.
	a.holdRepoLists([]string{"repo-x"}, []string{"repo-y"})
	a.holdRepoLists([]string{"repo-y"}, nil)
	pushed, failed = a.takePendingRepoLists()
	require.Equal(t, []string{"repo-x", "repo-y"}, pushed)
	require.Nil(t, failed, "a hold that pushes a held failure must cancel it, not keep both")

	pushed, failed = a.takePendingRepoLists()
	require.Nil(t, pushed, "take must clear")
	require.Nil(t, failed, "take must clear")
}

// THE SUPPRESSED CALLBACK IS THE ONLY EGRESS FOR THE REPO LISTS TOO.
//
// The reproduction from #191. Turn N pushes A,B and fails C, then its
// submit_outcome is rejected and the callback is suppressed - rec, and both
// lists with it, are discarded. Turn N+1 cannot recover them: every repo has a
// clean index, so CommitAndPush short-circuits and the repo appears in neither
// list (internal/bootstrap/repo.go's documented KNOWN LIMITATION). The two
// behaviours compose into total loss rather than one-turn staleness.
func TestFinalizeTurn_OutcomeRepromptDoesNotLoseRepoLists(t *testing.T) {
	h := newRepoListHarness(t)
	h.suppressed(t, "turn-1", []string{"repo-a", "repo-b"}, []string{"repo-c"})

	// Turn 2: clean index everywhere, so this turn's own lists are empty.
	body := h.clean(t, "turn-2", nil, nil)

	require.Equal(t, []any{"repo-a", "repo-b"}, body["pushedRepos"],
		"a suppressed turn's pushes have no other wire path")
	require.Equal(t, []any{"repo-c"}, body["failedRepos"],
		"a repo that failed to push holds commits that exist nowhere but a disk about to disappear")
}

// Pre-mortem 1: a held failedRepos that survives the success that fixed it is
// worse than the bug - the operator reads it as an instruction to redo work
// that has since landed.
func TestFinalizeTurn_HeldFailureDroppedByLaterPush(t *testing.T) {
	h := newRepoListHarness(t)
	h.suppressed(t, "turn-1", nil, []string{"repo-c"})

	// The re-prompted turn pushes the repo that failed before.
	body := h.clean(t, "turn-2", []string{"repo-c"}, nil)

	require.Equal(t, []any{"repo-c"}, body["pushedRepos"])
	require.NotContains(t, body, "failedRepos",
		"a held failure must be cancelled by a later push, not reported as lost work")
}

// Pre-mortem 6: maxOutcomeReprompts allows more than one suppression, so the
// hold must accumulate across a chain rather than overwrite.
func TestFinalizeTurn_ChainedRepromptsAccumulateRepoLists(t *testing.T) {
	h := newRepoListHarness(t)
	h.suppressed(t, "turn-1", []string{"repo-a"}, nil)
	h.suppressed(t, "turn-2", []string{"repo-b"}, []string{"repo-c"})

	body := h.clean(t, "turn-3", []string{"repo-d"}, nil)

	require.Equal(t, []any{"repo-a", "repo-b", "repo-d"}, body["pushedRepos"],
		"a second suppression must not drop the first suppression's pushes")
	require.Equal(t, []any{"repo-c"}, body["failedRepos"])
}

// The chain does not always end with a clean turn: once maxOutcomeReprompts is
// spent, reprompt() refuses and finalizeTurn falls THROUGH the suppression
// block to deliver the callback so the operator's own cap takes over. That
// fall-through is the last exit the held lists have, so it must carry them.
func TestFinalizeTurn_BudgetExhaustedStillDeliversTheHeldRepoLists(t *testing.T) {
	h := newRepoListHarness(t)
	h.suppressed(t, "turn-1", []string{"repo-a"}, []string{"repo-c"})
	h.suppressed(t, "turn-2", []string{"repo-b"}, nil)

	// Third rejected outcome: the re-prompt budget is spent, so this turn is
	// NOT suppressed - it delivers, and it is the only turn that can.
	h.mgr.SetTranscriptPathForTest(rejectedOutcomeTranscript(t))
	rec := &turn.Record{ID: "turn-3", CallbackURL: h.url}
	h.app.finalizeTurn(rec, config{}, metrics.New(prometheus.NewRegistry()),
		obs.NewLogger(h.logs, slog.LevelInfo), h.sender, "")

	select {
	case body := <-h.delivered:
		require.Equal(t, []any{"repo-a", "repo-b"}, body["pushedRepos"],
			"the budget-exhausted fall-through is the held lists' last exit")
		require.Equal(t, []any{"repo-c"}, body["failedRepos"])
	case <-time.After(2 * time.Second):
		t.Fatal("a budget-exhausted turn must deliver its callback")
	}
}

// A turn that pushed nothing and was not suppressed must be byte-identical on
// the wire to what it is today: omitempty drops both keys.
func TestFinalizeTurn_NoHeldListsLeavesThePayloadUnchanged(t *testing.T) {
	h := newRepoListHarness(t)
	body := h.clean(t, "turn-1", nil, nil)
	require.NotContains(t, body, "pushedRepos")
	require.NotContains(t, body, "failedRepos")
}

// C1: the suppression log line is the only channel that provably escapes an
// agent pod, and it is what let #191 attribute the one live occurrence. The
// held repo lists are named on it beside held_internal_issues.
func TestFinalizeTurn_SuppressionLogNamesTheHeldRepoLists(t *testing.T) {
	h := newRepoListHarness(t)
	h.suppressed(t, "turn-1", []string{"repo-a", "repo-b"}, []string{"repo-c"})

	var line map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(h.logs.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			continue
		}
		if entry["msg"] == "re-prompted agent after rejected outcome tool; suppressing this turn's callback" {
			line = entry
		}
	}
	require.NotNil(t, line, "the suppression log line must be emitted")
	require.Equal(t, []any{"repo-a", "repo-b"}, line["held_pushed_repos"])
	require.Equal(t, []any{"repo-c"}, line["held_failed_repos"])
}
