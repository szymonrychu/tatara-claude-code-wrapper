package transcript

import (
	"log/slog"
	"sync"
	"testing"
	"time"
)

const w3ProbeText = "Do not stop. Reply with exactly TATARA-ALIVE <one sentence>."

// activityRecorder captures every WithActivity call so a test can assert not
// just how many fired but which of them were flagged probe-originated.
type activityRecorder struct {
	mu     sync.Mutex
	fired  int
	probed int
}

func (a *activityRecorder) hook(_ string, probeOriginated bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fired++
	if probeOriginated {
		a.probed++
	}
}

func (a *activityRecorder) counts() (fired, probed int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fired, a.probed
}

// newProbeTailer builds a Tailer wired to a registered probe and an activity
// recorder, ready to be driven line by line through processLine.
func newProbeTailer(t *testing.T, probeText string) (*Tailer, *ProbeTracker, *activityRecorder, *[]string) {
	t.Helper()
	tracker := NewProbeTracker()
	var outMu sync.Mutex
	outcomes := []string{}
	tracker.WithOutcomeObserver(func(outcome string, _ time.Duration) {
		outMu.Lock()
		outcomes = append(outcomes, outcome)
		outMu.Unlock()
	})
	if probeText != "" {
		tracker.Register("probe-1", probeText, time.Date(2026, 8, 9, 17, 33, 7, 0, time.UTC))
	}
	rec := &activityRecorder{}
	tailer := NewTailer(slog.New(newCaptureHandler()), NewRedactor(nil), func() string { return "turn-1" })
	tailer.WithActivity(rec.hook)
	tailer.WithProbes(tracker)
	return tailer, tracker, rec, &outcomes
}

// TestTailer_ProbeEnqueueDoesNotFireActivity is the regression test for the
// bug the spike found: processLine fires the activity hook for EVERY non-empty
// line, and the turn timer that hook resets is an INACTIVITY timer. So the
// probe's own transcript line was resetting the deadline of the very turn it
// was sent to investigate - probing a hung agent would have kept it alive for
// as long as the operator kept asking, and the more suspicious a turn looked
// the longer it would have survived.
//
// The exclusion has to be narrow, so this also pins that an enqueue for any
// OTHER message still counts as activity.
func TestTailer_ProbeEnqueueDoesNotFireActivity(t *testing.T) {
	tests := []struct {
		name       string
		probeText  string
		fixture    string
		wantFired  int
		wantProbed int
	}{
		{
			name:       "the probe's own enqueue is excluded",
			probeText:  w3ProbeText,
			fixture:    "probe_lifecycle_remove.jsonl",
			wantFired:  4,
			wantProbed: 1,
		},
		{
			name:      "an enqueue for someone else's message is ordinary activity",
			probeText: w3ProbeText,
			fixture:   "queue_enqueue_other_message.jsonl",
			// Fires, and is NOT flagged: only the registered probe text is
			// excluded, so no other queue source can silently stop the
			// inactivity timer from working.
			wantFired:  1,
			wantProbed: 0,
		},
		{
			name:       "with no probe registered nothing is ever excluded",
			probeText:  "",
			fixture:    "probe_lifecycle_remove.jsonl",
			wantFired:  4,
			wantProbed: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tailer, _, rec, _ := newProbeTailer(t, tc.probeText)
			for _, line := range readFixtureLines(t, tc.fixture) {
				tailer.processLine([]byte(line))
			}
			fired, probed := rec.counts()
			if fired != tc.wantFired {
				t.Errorf("activity fired %d times, want %d", fired, tc.wantFired)
			}
			if probed != tc.wantProbed {
				t.Errorf("probe-originated activity fired %d times, want %d", probed, tc.wantProbed)
			}
		})
	}
}

// TestTailer_ProbeLifecycle drives the whole state machine off golden
// transcript lines.
//
// The removal cases are the ones that matter. A real `remove` line usually
// carries NO content (verified on-disk: 2469 enqueue / 1156 remove / 1266
// dequeue), and `dequeue` is an undocumented third operation that behaves the
// same way and is roughly as common. A tracker that identified the probe by
// content on the removal side would therefore match neither, leave every probe
// pending forever, and report a permanent false stall for a perfectly healthy
// agent.
func TestTailer_ProbeLifecycle(t *testing.T) {
	tests := []struct {
		name         string
		fixture      string
		wantState    ProbeState
		wantAnswer   string
		wantOutcomes []string
	}{
		{
			name:         "content-less dequeue delivers, assistant marker answers",
			fixture:      "probe_lifecycle_dequeue.jsonl",
			wantState:    ProbeStateAnswered,
			wantAnswer:   "TATARA-ALIVE I am running the wrapper test suite and waiting on go test.",
			wantOutcomes: []string{ProbeOutcomeAnswered},
		},
		{
			name:         "content-less remove delivers; the user echo does not answer",
			fixture:      "probe_lifecycle_remove.jsonl",
			wantState:    ProbeStateAnswered,
			wantAnswer:   "TATARA-ALIVE rebasing the feature branch onto main.",
			wantOutcomes: []string{ProbeOutcomeAnswered},
		},
		{
			name:      "enqueue with no removal stays pending: blocked inside one long tool call",
			fixture:   "probe_enqueue_only.jsonl",
			wantState: ProbeStatePending,
			// Deliberately no outcome yet. Pending is a live diagnosis, not a
			// finished one, and counting it early would erase the distinction
			// between "never reached a tool-call boundary" and "answered late".
			wantOutcomes: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tailer, tracker, _, outcomes := newProbeTailer(t, w3ProbeText)
			for _, line := range readFixtureLines(t, tc.fixture) {
				tailer.processLine([]byte(line))
			}
			st, ok := tracker.Status("probe-1")
			if !ok {
				t.Fatal("probe-1 is not tracked")
			}
			if st.State != tc.wantState {
				t.Errorf("state = %q, want %q", st.State, tc.wantState)
			}
			if st.Answer != tc.wantAnswer {
				t.Errorf("answer = %q, want %q", st.Answer, tc.wantAnswer)
			}
			if len(*outcomes) != len(tc.wantOutcomes) {
				t.Fatalf("outcomes = %v, want %v", *outcomes, tc.wantOutcomes)
			}
			for i, want := range tc.wantOutcomes {
				if (*outcomes)[i] != want {
					t.Errorf("outcome[%d] = %q, want %q", i, (*outcomes)[i], want)
				}
			}
		})
	}
}

// TestTailer_ProbeEnqueueStampsEnqueuedAt: the enqueue timestamp is what makes
// "pending" readable. The spike measured a 70s sleep buffering a probe for
// 58.2s while its enqueue line appeared immediately, so an EnqueuedAt that is
// set while the state is still pending is positive evidence that the CLI took
// the message and the turn simply has not reached a tool-call boundary.
func TestTailer_ProbeEnqueueStampsEnqueuedAt(t *testing.T) {
	tailer, tracker, _, _ := newProbeTailer(t, w3ProbeText)
	for _, line := range readFixtureLines(t, "probe_enqueue_only.jsonl") {
		tailer.processLine([]byte(line))
	}
	st, ok := tracker.Status("probe-1")
	if !ok {
		t.Fatal("probe-1 is not tracked")
	}
	if st.State != ProbeStatePending {
		t.Fatalf("state = %q, want pending", st.State)
	}
	if st.EnqueuedAt.IsZero() {
		t.Fatal("EnqueuedAt not stamped: pending is indistinguishable from never-written")
	}
	if !st.DeliveredAt.IsZero() {
		t.Errorf("DeliveredAt = %v, want zero", st.DeliveredAt)
	}
}

// TestProbeTracker_OutcomesOnSupersedeAndFinalize pins the terminal accounting.
// The last probe of a pod's life is exactly the one sent to the turn that got
// the pod stopped, so if it never reached the counter the metric would
// systematically censor the failures it exists to show.
func TestProbeTracker_OutcomesOnSupersedeAndFinalize(t *testing.T) {
	var mu sync.Mutex
	var got []string
	tr := NewProbeTracker().WithOutcomeObserver(func(outcome string, _ time.Duration) {
		mu.Lock()
		got = append(got, outcome)
		mu.Unlock()
	})
	t0 := time.Date(2026, 8, 9, 17, 0, 0, 0, time.UTC)

	// Probe 1 never leaves pending.
	tr.Register("p1", "text-1", t0)
	// Probe 2 supersedes it and IS delivered, but never answered.
	tr.Register("p2", "text-2", t0.Add(time.Minute))
	tr.NoteRemoval(t0.Add(90 * time.Second))
	// Shutdown closes probe 2 out.
	tr.Finalize()
	// Finalize is idempotent.
	tr.Finalize()

	mu.Lock()
	defer mu.Unlock()
	want := []string{ProbeOutcomeNeverDelivered, ProbeOutcomeDeliveredUnanswered}
	if len(got) != len(want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("outcome[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestProbeTracker_AnswerLatency: the answer clock runs from the write, not
// from delivery. Time spent buffered inside a long tool call is the dominant
// term (58.2s of a 70s block in the spike) and is precisely what the operator
// needs to see.
func TestProbeTracker_AnswerLatency(t *testing.T) {
	var mu sync.Mutex
	var gotLatency time.Duration
	tr := NewProbeTracker().WithOutcomeObserver(func(_ string, l time.Duration) {
		mu.Lock()
		gotLatency = l
		mu.Unlock()
	})
	t0 := time.Date(2026, 8, 9, 17, 0, 0, 0, time.UTC)
	tr.Register("p1", "text-1", t0)
	tr.NoteRemoval(t0.Add(58 * time.Second))
	latency, ok := tr.NoteAssistantText("TATARA-ALIVE compiling", t0.Add(59*time.Second))
	if !ok {
		t.Fatal("marker text did not answer a delivered probe")
	}
	if latency != 59*time.Second {
		t.Errorf("latency = %v, want 59s (measured from the write, not from delivery)", latency)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotLatency != 59*time.Second {
		t.Errorf("observed latency = %v, want 59s", gotLatency)
	}
}

// TestProbeTracker_AnswerRequiresDelivery: text arriving before the probe was
// handed to the model cannot be an answer to it, however it is worded.
func TestProbeTracker_AnswerRequiresDelivery(t *testing.T) {
	tr := NewProbeTracker()
	t0 := time.Date(2026, 8, 9, 17, 0, 0, 0, time.UTC)
	tr.Register("p1", "text-1", t0)

	if _, ok := tr.NoteAssistantText("TATARA-ALIVE still here", t0.Add(time.Second)); ok {
		t.Fatal("an undelivered probe was answered")
	}
	tr.NoteRemoval(t0.Add(10 * time.Second))
	if _, ok := tr.NoteAssistantText("TATARA-ALIVE stale", t0.Add(5*time.Second)); ok {
		t.Fatal("text written before delivery answered the probe")
	}
	if _, ok := tr.NoteAssistantText("Sure, I'll do that.", t0.Add(11*time.Second)); ok {
		t.Fatal("ordinary assistant text answered the probe without the marker")
	}
	if _, ok := tr.NoteAssistantText("  TATARA-ALIVE running tests", t0.Add(12*time.Second)); !ok {
		t.Fatal("leading whitespace defeated the marker match")
	}
}

// TestProbeTracker_NoteEnqueueMatching pins how narrow the activity exclusion
// is: only the registered probe's own text, and only for an enqueue.
func TestProbeTracker_NoteEnqueueMatching(t *testing.T) {
	t0 := time.Date(2026, 8, 9, 17, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		register string
		content  string
		want     bool
	}{
		{"exact match", "probe me", "probe me", true},
		{"whitespace tolerated", "probe me", "  probe me\n", true},
		{"different message", "probe me", "do the other thing", false},
		{"prefix is not enough", "probe me", "probe me and also deploy", false},
		{"empty content", "probe me", "", false},
		{"no probe registered", "", "probe me", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewProbeTracker()
			if tc.register != "" {
				tr.Register("p1", tc.register, t0)
			}
			if got := tr.NoteEnqueue(tc.content, t0); got != tc.want {
				t.Errorf("NoteEnqueue(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}
