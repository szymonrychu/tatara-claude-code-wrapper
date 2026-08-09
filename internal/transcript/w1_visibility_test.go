package transcript

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// fakeStreamCounter is a test double for StreamCounter recording the
// stream_type label of every increment.
type fakeStreamCounter struct {
	mu    sync.Mutex
	types []string
}

func (f *fakeStreamCounter) WithLabelValues(lvs ...string) prometheus.Counter {
	f.mu.Lock()
	if len(lvs) > 0 {
		f.types = append(f.types, lvs[0])
	}
	f.mu.Unlock()
	return &noopCounter{}
}

func (f *fakeStreamCounter) Types() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.types...)
}

// fakeQueueOpsCounter is a test double for QueueOpsCounter recording the {op}
// label of every increment, in order.
type fakeQueueOpsCounter struct {
	mu  sync.Mutex
	ops []string
}

func (f *fakeQueueOpsCounter) WithLabelValues(lvs ...string) prometheus.Counter {
	f.mu.Lock()
	op := ""
	if len(lvs) > 0 {
		op = lvs[0]
	}
	f.ops = append(f.ops, op)
	f.mu.Unlock()
	return &noopCounter{}
}

func (f *fakeQueueOpsCounter) Ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

// readFixtureLines reads a golden JSONL fixture from this package's testdata,
// dropping blank lines.
func readFixtureLines(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, l)
	}
	return out
}

// step is an assertion taken after a given number of fixture lines have been
// processed, so an "outstanding across N lines" claim is checked at each N and
// not only at the end.
type step struct {
	afterLine    int
	wantTasks    int64
	wantPending  bool
	wantPendingT string // RFC3339; checked only when wantPending is true
}

// TestTailer_W1Visibility drives golden JSONL fixtures through processLine and
// pins the two things the wrapper was previously blind to: `queue-operation`
// entries (an enqueue with no matching remove is a turn wedged inside one long
// tool call) and outstanding `Task` tool_use blocks (a subagent is running,
// even though the parent transcript has gone quiet).
func TestTailer_W1Visibility(t *testing.T) {
	cases := []struct {
		name        string
		fixture     string
		wantOps     []string
		wantStream  []string
		steps       []step
		checkRecord func(t *testing.T, recs []map[string]any)
	}{
		{
			name:       "enqueue without remove stays pending",
			fixture:    "queue_enqueue_only.jsonl",
			wantOps:    []string{"enqueue"},
			wantStream: []string{"queue-operation"},
			steps: []step{
				{afterLine: 1, wantTasks: 0, wantPending: true, wantPendingT: "2026-08-09T17:33:07Z"},
			},
			checkRecord: func(t *testing.T, recs []map[string]any) {
				t.Helper()
				qs := filterByAction(recs, "agent_stream_queue_operation")
				if len(qs) != 1 {
					t.Fatalf("queue-operation log records = %d, want 1", len(qs))
				}
				if qs[0]["level"] != "INFO" {
					t.Errorf("queue-operation logged at %v, want INFO", qs[0]["level"])
				}
				if qs[0]["op"] != "enqueue" {
					t.Errorf("op = %v, want enqueue", qs[0]["op"])
				}
			},
		},
		{
			name:       "enqueue and remove clears pending",
			fixture:    "queue_enqueue_remove.jsonl",
			wantOps:    []string{"enqueue", "remove"},
			wantStream: []string{"queue-operation", "queue-operation"},
			steps: []step{
				{afterLine: 1, wantPending: true, wantPendingT: "2026-08-09T17:33:07Z"},
				// The real `remove` line carries no content at all, so a
				// tracker keyed strictly on content equality would leave this
				// pending forever and report a permanent false stall.
				{afterLine: 2, wantPending: false},
			},
		},
		{
			name:       "sidechain entry is attributed to its parent",
			fixture:    "sidechain_entry.jsonl",
			wantOps:    nil,
			wantStream: []string{"text"},
			steps:      []step{{afterLine: 1}},
			checkRecord: func(t *testing.T, recs []map[string]any) {
				t.Helper()
				sc := filterByAction(recs, "agent_stream_sidechain")
				if len(sc) != 1 {
					t.Fatalf("sidechain log records = %d, want 1", len(sc))
				}
				if sc[0]["is_sidechain"] != true {
					t.Errorf("is_sidechain = %v, want true", sc[0]["is_sidechain"])
				}
				if sc[0]["parent_uuid"] != "uuid-parent-1" {
					t.Errorf("parent_uuid = %v, want uuid-parent-1", sc[0]["parent_uuid"])
				}
			},
		},
		{
			name:       "task outstanding across every intervening line",
			fixture:    "task_outstanding.jsonl",
			wantOps:    nil,
			wantStream: []string{"tool_use", "text", "text", "text", "tool_result"},
			steps: []step{
				{afterLine: 1, wantTasks: 1},
				{afterLine: 2, wantTasks: 1},
				{afterLine: 3, wantTasks: 1},
				{afterLine: 4, wantTasks: 1},
				{afterLine: 5, wantTasks: 0},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newCaptureHandler()
			qc := &fakeQueueOpsCounter{}
			sc := &fakeStreamCounter{}
			tl := NewTailer(slog.New(h), NewRedactor(nil), func() string { return "turn-1" })
			tl.WithQueueOpsCounter(qc)
			tl.WithCounter(sc)

			byLine := map[int][]step{}
			for _, s := range tc.steps {
				byLine[s.afterLine] = append(byLine[s.afterLine], s)
			}

			lines := readFixtureLines(t, tc.fixture)
			for i, line := range lines {
				tl.processLine([]byte(line + "\n"))
				for _, s := range byLine[i+1] {
					if got := tl.OutstandingTaskCalls(); got != s.wantTasks {
						t.Fatalf("after line %d: OutstandingTaskCalls = %d, want %d", i+1, got, s.wantTasks)
					}
					at, ok := tl.QueuePendingSince()
					if ok != s.wantPending {
						t.Fatalf("after line %d: queue pending = %v, want %v", i+1, ok, s.wantPending)
					}
					if s.wantPending && s.wantPendingT != "" {
						want, err := time.Parse(time.RFC3339, s.wantPendingT)
						if err != nil {
							t.Fatalf("bad fixture time %q: %v", s.wantPendingT, err)
						}
						if !at.Equal(want) {
							t.Errorf("after line %d: PendingSince = %s, want %s (the enqueue's own timestamp, not read time)", i+1, at, want)
						}
					}
				}
			}

			if got := qc.Ops(); !equalStrings(got, tc.wantOps) {
				t.Errorf("queue op labels = %v, want %v", got, tc.wantOps)
			}
			if got := sc.Types(); !equalStrings(got, tc.wantStream) {
				t.Errorf("stream_type labels = %v, want %v", got, tc.wantStream)
			}
			if tc.checkRecord != nil {
				tc.checkRecord(t, h.Records())
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestClampQueueOp pins the bounded {op} label set.
func TestClampQueueOp(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"enqueue", "enqueue"},
		{"remove", "remove"},
		{"dequeue", "dequeue"},
		{"", "other"},
		{"some-future-op", "other"},
	} {
		if got := clampQueueOp(tc.in); got != tc.want {
			t.Errorf("clampQueueOp(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestClampNonMessageType_QueueOperationNoLongerOther pins the metric-label
// widening: these three types are common in real transcripts and used to
// collapse into "other", which made "other" useless as an unknown-shape signal.
func TestClampNonMessageType_QueueOperationNoLongerOther(t *testing.T) {
	for _, want := range []string{"queue-operation", "attachment", "file-history-snapshot"} {
		if got := clampNonMessageType(want); got != want {
			t.Errorf("clampNonMessageType(%q) = %q, want %q", want, got, want)
		}
	}
	if got := clampNonMessageType("still-unknown"); got != "other" {
		t.Errorf("clampNonMessageType(still-unknown) = %q, want other", got)
	}
}

// TestQueueTracker pins the removal semantics that the real transcript shape
// forces: a content-less remove and a remove for an enqueue written before the
// tailer attached both have to drain the tracker, or PendingSince reports a
// stall that is not happening.
func TestQueueTracker(t *testing.T) {
	t0 := time.Unix(1000, 0)
	t1 := time.Unix(1010, 0)

	t.Run("matched key removes that item only", func(t *testing.T) {
		var q queueTracker
		q.enqueue("a", t0)
		q.enqueue("b", t1)
		q.remove("a")
		at, ok := q.PendingSince()
		if !ok || !at.Equal(t1) {
			t.Fatalf("PendingSince = %s,%v; want %s,true", at, ok, t1)
		}
	})

	t.Run("empty key clears everything", func(t *testing.T) {
		var q queueTracker
		q.enqueue("a", t0)
		q.enqueue("b", t1)
		q.remove("")
		if _, ok := q.PendingSince(); ok {
			t.Fatal("a content-less remove must drain the tracker")
		}
	})

	t.Run("unmatched key drops the oldest", func(t *testing.T) {
		var q queueTracker
		q.enqueue("a", t0)
		q.enqueue("b", t1)
		q.remove("never-enqueued")
		at, ok := q.PendingSince()
		if !ok || !at.Equal(t1) {
			t.Fatalf("PendingSince = %s,%v; want %s,true", at, ok, t1)
		}
	})

	t.Run("remove on empty tracker is a no-op", func(t *testing.T) {
		var q queueTracker
		q.remove("a")
		if _, ok := q.PendingSince(); ok {
			t.Fatal("empty tracker must report nothing pending")
		}
	})

	t.Run("pending is capped", func(t *testing.T) {
		var q queueTracker
		for i := 0; i < maxPendingQueueItems*2; i++ {
			q.enqueue("k", t0)
		}
		q.mu.Lock()
		n := len(q.pending)
		q.mu.Unlock()
		if n != maxPendingQueueItems {
			t.Fatalf("pending len = %d, want %d", n, maxPendingQueueItems)
		}
	})
}

// TestTailer_TaskCallsResetOnTurnChange verifies an unfinished Task cannot
// leak its outstanding count into the next turn: a subagent whose tool_result
// never arrived cannot outlive the turn that launched it, and a counter that
// only ever goes up would make every later turn look like it had subagents.
func TestTailer_TaskCallsResetOnTurnChange(t *testing.T) {
	h := newCaptureHandler()
	turnID := "turn-1"
	tl := NewTailer(slog.New(h), NewRedactor(nil), func() string { return turnID })

	tl.processLine([]byte(makeToolUseLineNamed("toolu_task_1", "Task") + "\n"))
	if got := tl.OutstandingTaskCalls(); got != 1 {
		t.Fatalf("OutstandingTaskCalls = %d, want 1", got)
	}

	turnID = "turn-2"
	tl.processLine([]byte(makeToolUseLineNamed("toolu_read_1", "Read") + "\n"))
	if got := tl.OutstandingTaskCalls(); got != 0 {
		t.Fatalf("OutstandingTaskCalls after turn change = %d, want 0", got)
	}
}

// TestTailer_TaskCallsNeverGoNegative verifies a tool_result with no matching
// Task tool_use (the tailer attached mid-turn, or the result belongs to some
// other tool) leaves the counter alone rather than driving it below zero.
func TestTailer_TaskCallsNeverGoNegative(t *testing.T) {
	h := newCaptureHandler()
	tl := NewTailer(slog.New(h), NewRedactor(nil), func() string { return "turn-1" })

	tl.processLine([]byte(makeToolResultLineFor("toolu_unknown", false) + "\n"))
	if got := tl.OutstandingTaskCalls(); got != 0 {
		t.Fatalf("OutstandingTaskCalls = %d, want 0", got)
	}

	tl.processLine([]byte(makeToolUseLineNamed("toolu_task_1", "Task") + "\n"))
	tl.processLine([]byte(makeToolResultLineFor("toolu_task_1", false) + "\n"))
	tl.processLine([]byte(makeToolResultLineFor("toolu_task_1", false) + "\n"))
	if got := tl.OutstandingTaskCalls(); got != 0 {
		t.Fatalf("OutstandingTaskCalls after duplicate result = %d, want 0", got)
	}
}

// TestTailer_QueueOperationsWithoutCounter verifies the tracker works with no
// counter wired: subagent tailers and CCW_LOG_TRANSCRIPT-less setups must not
// silently lose the queue signal because a metric happens to be absent.
func TestTailer_QueueOperationsWithoutCounter(t *testing.T) {
	h := newCaptureHandler()
	tl := NewTailer(slog.New(h), NewRedactor(nil), func() string { return "turn-1" })
	for _, line := range readFixtureLines(t, "queue_enqueue_only.jsonl") {
		tl.processLine([]byte(line + "\n"))
	}
	if _, ok := tl.QueuePendingSince(); !ok {
		t.Fatal("queue tracking must not depend on a counter being wired")
	}
}
