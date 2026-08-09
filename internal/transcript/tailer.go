package transcript

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

const pollInterval = 200 * time.Millisecond

// internalIssueToolName is the exact namespaced MCP tool name as Claude sees it.
// The cli server registers as "tatara", so block.Name == mcp__tatara__<tool>.
// If either the server name or tool name changes in the cli, the wrapper silently
// stops emitting; TestInternalIssueToolNameConst guards this coupling.
const internalIssueToolName = "mcp__tatara__report_internal_issue"

// knownIssueCategories is the fixed cardinality set for issue category labels.
var knownIssueCategories = map[string]bool{
	"tool_error": true, "directive_contradiction": true, "workspace_broken": true,
	"memory_inconsistent": true, "graph_inconsistent": true, "auth": true, "other": true,
}

// knownIssueSeverities is the fixed cardinality set for issue severity labels.
var knownIssueSeverities = map[string]bool{
	"warn": true, "error": true,
}

// maxPartialBytes caps the in-memory accumulator for partial (non-newline-
// terminated) lines. Matches the 16 MiB scanner limit used by the stop-hook
// reader in transcript.go. Exceeding this emits a raw event and resets
// partial so a pathological write cannot grow memory unbounded.
const maxPartialBytes = 16 * 1024 * 1024

// StreamCounter is satisfied by *prometheus.CounterVec.
type StreamCounter interface {
	WithLabelValues(lvs ...string) prometheus.Counter
}

// InternalIssueCounter is satisfied by *prometheus.CounterVec with labels
// {category, severity}. It is a distinct interface from StreamCounter so the
// call site is explicit about the 2-label shape.
type InternalIssueCounter interface {
	WithLabelValues(lvs ...string) prometheus.Counter
}

// ToolCallsCounter is satisfied by *prometheus.CounterVec with labels
// {tool, outcome}. Distinct interface from StreamCounter so the 2-label shape
// is explicit at the call site (issue #51).
type ToolCallsCounter interface {
	WithLabelValues(lvs ...string) prometheus.Counter
}

// QueueOpsCounter is satisfied by *prometheus.CounterVec with label {op}. It
// counts `queue-operation` transcript entries, which are how Claude Code
// records a message being parked while a turn is mid-tool-call and later
// delivered. Distinct interface from StreamCounter so the 1-label {op} shape
// is explicit at the call site.
type QueueOpsCounter interface {
	WithLabelValues(lvs ...string) prometheus.Counter
}

// tataraToolPrefix is the namespace every tatara MCP tool carries as Claude
// sees it (the cli server registers as "tatara"). Any tool under this prefix
// is the platform's own bounded surface, so it is kept verbatim as the metric
// label; see clampToolName.
const tataraToolPrefix = "mcp__tatara__"

// knownBuiltinTools is the fixed cardinality set of built-in Claude Code tool
// names kept verbatim in the ccw_tool_calls_total{tool} label. Everything not
// here and not under tataraToolPrefix clamps to "other" so an arbitrary MCP
// server cannot blow up label cardinality (rule 13).
var knownBuiltinTools = map[string]bool{
	"Bash": true, "Read": true, "Edit": true, "Write": true, "Glob": true,
	"Grep": true, "Task": true, "TodoWrite": true, "WebFetch": true,
	"WebSearch": true, "NotebookEdit": true,
}

// clampToolName bounds the ccw_tool_calls_total{tool} label. Built-in tools and
// the tatara MCP surface (a platform-bounded namespace) are kept verbatim;
// everything else - notably arbitrary third-party MCP servers - collapses to
// "other". This mirrors clampNonMessageType and keeps cardinality bounded while
// still giving per-tool failure rates for the tools the loop depends on.
func clampToolName(name string) string {
	if knownBuiltinTools[name] {
		return name
	}
	if strings.HasPrefix(name, tataraToolPrefix) {
		return name
	}
	return "other"
}

// taskToolName is the built-in tool Claude Code uses to launch a subagent.
// An outstanding Task call is the parent turn's only in-band evidence that
// work is happening in a subagent transcript rather than in its own file.
const taskToolName = "Task"

// knownQueueOps is the fixed cardinality set for the ccw_queue_operations_total
// {op} label. Observed in real transcripts: `enqueue` (a message parked while
// the turn is mid-tool-call), `remove` and `dequeue` (it left the queue).
var knownQueueOps = map[string]bool{
	"enqueue": true, "remove": true, "dequeue": true,
}

// clampQueueOp bounds the ccw_queue_operations_total{op} label.
func clampQueueOp(op string) string {
	if knownQueueOps[op] {
		return op
	}
	return "other"
}

// maxPendingQueueItems bounds the queueTracker's pending slice. A transcript
// is read from the start on every (re)open, so an enqueue whose remove was
// written before the tailer attached can linger; the cap makes that leak
// bounded rather than unbounded.
const maxPendingQueueItems = 128

type queuedItem struct {
	key string
	at  time.Time
}

// queueTracker records `queue-operation` enqueues that have not yet been
// matched by a remove/dequeue. An enqueue with no matching removal is the
// signal that a message was accepted by the CLI but never delivered to the
// model - i.e. the turn is blocked inside a single long tool call. In this
// phase nothing acts on it; it is exposed through PendingSince for later
// phases and for tests.
//
// Guarded by its own mutex: it is written from the tailer goroutine and read
// from whichever goroutine asks for a snapshot.
type queueTracker struct {
	mu      sync.Mutex
	pending []queuedItem
}

// enqueue records that key entered the queue at at.
func (q *queueTracker) enqueue(key string, at time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.pending, queuedItem{key: key, at: at})
	if len(q.pending) > maxPendingQueueItems {
		q.pending = q.pending[len(q.pending)-maxPendingQueueItems:]
	}
}

// remove drops the oldest pending item matching key. A real `remove` line
// frequently carries NO content (verified against on-disk transcripts), in
// which case key is "" and the whole pending set is cleared: the queue
// drained and there is no way to tell which item left. An unmatched non-empty
// key also drops the oldest entry rather than leaving it pending forever,
// since the only way to see a removal for an unseen enqueue is to have
// attached to the transcript after that enqueue was written.
func (q *queueTracker) remove(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return
	}
	if key == "" {
		q.pending = nil
		return
	}
	for i, it := range q.pending {
		if it.key == key {
			q.pending = append(q.pending[:i:i], q.pending[i+1:]...)
			return
		}
	}
	q.pending = q.pending[1:]
}

// PendingSince returns the enqueue time of the oldest queue entry that has
// not been removed, and whether there is one at all.
func (q *queueTracker) PendingSince() (time.Time, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return time.Time{}, false
	}
	return q.pending[0].at, true
}

// Tailer reads a JSONL transcript file from the start and follows appends.
// It re-opens the file on inode change (claude restart) and never drops a
// malformed line (emits a raw event instead).
type Tailer struct {
	log                  *slog.Logger
	redactor             *Redactor
	turnID               func() string
	counter              StreamCounter
	internalIssueCounter InternalIssueCounter
	toolCallsCounter     ToolCallsCounter
	queueOpsCounter      QueueOpsCounter
	onActivity           func(turnID string, probeOriginated bool)

	// probes, when set, follows an in-flight liveness probe through the
	// transcript. Owned by the session Manager (it must survive
	// CCW_LOG_TRANSCRIPT=false, which means no Tailer at all), so this is a
	// borrowed reference and may be nil.
	probes *ProbeTracker

	// queue records unmatched `queue-operation` enqueues. Observation only in
	// this phase - nothing reads it to make a decision yet.
	queue queueTracker

	// taskUseIDs holds the ids of `Task` tool_use blocks whose tool_result has
	// not arrived, i.e. subagents believed to still be running. Deliberately
	// NOT keyed off toolNames: that map exists only when WithToolCallsCounter
	// is wired, so keying off it would make subagent visibility depend on an
	// unrelated metric being enabled. Written and read only from the
	// processLine goroutine; taskCalls mirrors len(taskUseIDs) as an atomic so
	// OutstandingTaskCalls can be read from any goroutine.
	taskUseIDs map[string]struct{}
	taskTurnID string
	taskCalls  atomic.Int64

	// toolNames correlates a tool_use_id to its clamped tool name so a later
	// tool_result (which carries only the id) can be attributed to a tool for
	// ccw_tool_calls_total. Written on tool_use, read+deleted on tool_result,
	// and cleared on turn change so an orphaned tool_use cannot grow it
	// unbounded. processLine is single-goroutine, so no locking is needed.
	toolNames map[string]string
	tcTurnID  string

	// iiMu guards internalIssues/iiTurnID. Unlike toolNames (tailer-goroutine-
	// only), these are also read by DrainInternalIssues from the OnTurnDone
	// finalisation goroutine in cmd/wrapper/app.go, which runs concurrently
	// with Follow processing later transcript lines.
	iiMu           sync.Mutex
	internalIssues []turn.InternalIssueReport
	iiTurnID       string

	// readPos is the number of bytes Follow has fully processed (handed to
	// and returned from processLine) from the current transcript file (reset
	// to 0 on open/reopen) - never bytes merely read into a buffer, since a
	// trailing line with no newline yet is buffered in `partial` but not
	// processed. Used by CaughtUpTo so a turn-boundary drain can wait for the
	// tailer to have processed everything on disk as of a stat taken at the
	// drain call, closing the poll-interval race where DrainInternalIssues
	// runs before Follow's next poll has read the turn's final line (issue
	// found on PR #105), and the worse race where readPos was previously
	// advanced before processing, letting the drain believe it had caught up
	// while the turn's last report_internal_issue sat unprocessed in
	// `partial` (issue found in review of that fix).
	readPos atomic.Int64
}

// NewTailer constructs a Tailer. turnID is called per event to get the current
// in-flight turn id (may return ""). counter may be nil (no metrics).
func NewTailer(log *slog.Logger, redactor *Redactor, turnID func() string) *Tailer {
	return &Tailer{
		log: log, redactor: redactor, turnID: turnID,
		taskUseIDs: make(map[string]struct{}),
	}
}

// WithCounter attaches a prometheus-compatible counter. Returns self for chaining.
func (t *Tailer) WithCounter(c StreamCounter) *Tailer {
	t.counter = c
	return t
}

// WithInternalIssueCounter attaches a 2-label {category,severity} counter for
// report_internal_issue tool calls. nil-safe, returns self for chaining.
func (t *Tailer) WithInternalIssueCounter(c InternalIssueCounter) *Tailer {
	t.internalIssueCounter = c
	return t
}

// WithToolCallsCounter attaches a 2-label {tool,outcome} counter for agent
// tool calls (issue #51). nil-safe, returns self for chaining.
func (t *Tailer) WithToolCallsCounter(c ToolCallsCounter) *Tailer {
	t.toolCallsCounter = c
	t.toolNames = make(map[string]string)
	return t
}

// WithQueueOpsCounter attaches a 1-label {op} counter for `queue-operation`
// transcript entries. nil-safe, returns self for chaining.
func (t *Tailer) WithQueueOpsCounter(c QueueOpsCounter) *Tailer {
	t.queueOpsCounter = c
	return t
}

// OutstandingTaskCalls returns the number of `Task` tool_use blocks seen with
// no matching tool_result: the count of subagents this transcript believes are
// still running. Safe to call from any goroutine.
func (t *Tailer) OutstandingTaskCalls() int64 {
	return t.taskCalls.Load()
}

// QueuePendingSince returns the enqueue time of the oldest `queue-operation`
// entry with no matching removal, and whether there is one. Safe to call from
// any goroutine.
func (t *Tailer) QueuePendingSince() (time.Time, bool) {
	return t.queue.PendingSince()
}

// WithActivity attaches a hook fired once per processed transcript line that
// carries an in-flight turn id. It is the per-turn liveness heartbeat the
// session uses to reset its inactivity deadline. Returns self for chaining.
//
// probeOriginated marks a line the wrapper's own liveness probe caused. The
// turn deadline is an INACTIVITY timer, so without that flag a probe would
// reset the deadline of the very turn it is investigating - probing a hung
// agent would keep it alive indefinitely, which is the exact opposite of the
// point. See classifyProbeEnqueue for how narrow the flag is.
func (t *Tailer) WithActivity(fn func(turnID string, probeOriginated bool)) *Tailer {
	t.onActivity = fn
	return t
}

// WithProbes attaches the session's ProbeTracker so probe lifecycle
// transitions are observed off the transcript. nil-safe, returns self for
// chaining.
func (t *Tailer) WithProbes(p *ProbeTracker) *Tailer {
	t.probes = p
	return t
}

func (t *Tailer) incCounter(streamType string) {
	if t.counter != nil {
		t.counter.WithLabelValues(streamType).Inc() //nolint:errcheck
	}
}

// internalIssueInput is the JSON-deserialized body of a report_internal_issue call.
type internalIssueInput struct {
	Category      string `json:"category"`
	Severity      string `json:"severity"`
	Description   string `json:"description"`
	OffendingTool string `json:"offending_tool"`
	ResourceID    string `json:"resource_id"`
}

// emitInternalIssue is called after the generic tool_use INFO log to emit the
// additional ERROR/WARN log and increment the internal-issue counter. rawInput is
// the raw JSON from the transcript; free-text fields are scrubbed here before
// logging. Never panics.
func (t *Tailer) emitInternalIssue(turnID string, rawInput json.RawMessage) {
	var in internalIssueInput
	if err := json.Unmarshal(rawInput, &in); err != nil {
		// Parse failure: log ERROR with parse_error field, still count.
		t.log.Error("internal issue report",
			"action", "internal_issue_report",
			"category", "other",
			"severity", "error",
			"parse_error", err.Error(),
			"turn_id", turnID,
		)
		if t.internalIssueCounter != nil {
			t.internalIssueCounter.WithLabelValues("other", "error").Inc() //nolint:errcheck
		}
		t.accumulateInternalIssue(turnID, turn.InternalIssueReport{
			Category: "other", Severity: "error", Description: "internal issue report: unparseable input: " + err.Error(),
		})
		return
	}

	// Scrub free-text fields before logging; Category/Severity are clamped to
	// known enums and need no scrub.
	in.Description = t.redactor.Scrub(in.Description)
	in.OffendingTool = t.redactor.Scrub(in.OffendingTool)
	in.ResourceID = t.redactor.Scrub(in.ResourceID)

	// Clamp severity for the metric label; default to "error" on missing/unknown.
	metricSeverity := in.Severity
	if !knownIssueSeverities[metricSeverity] {
		metricSeverity = "error"
	}

	// Clamp category for the metric label; default to "other" on unknown.
	metricCategory := in.Category
	if !knownIssueCategories[metricCategory] {
		metricCategory = "other"
	}

	// Log at the level matching severity. Log carries raw (unclamped) values for
	// full queryability; only the counter uses clamped labels.
	logArgs := []any{
		"action", "internal_issue_report",
		"category", in.Category,
		"severity", metricSeverity,
		"description", in.Description,
		"turn_id", turnID,
	}
	if in.OffendingTool != "" {
		logArgs = append(logArgs, "offending_tool", in.OffendingTool)
	}
	if in.ResourceID != "" {
		logArgs = append(logArgs, "resource_id", in.ResourceID)
	}

	if metricSeverity == "warn" {
		t.log.Warn("internal issue report", logArgs...)
	} else {
		t.log.Error("internal issue report", logArgs...)
	}

	if t.internalIssueCounter != nil {
		t.internalIssueCounter.WithLabelValues(metricCategory, metricSeverity).Inc() //nolint:errcheck
	}

	// Accumulate the clamped-label report for the operator callback (fix: agent
	// pods are not Loki-scraped, so this is the only path the description
	// reaches an alertable, collected log stream). Category/Severity use the
	// already-clamped values so the operator's severity=="error" gate is exact.
	t.accumulateInternalIssue(turnID, turn.InternalIssueReport{
		Category: metricCategory, Severity: metricSeverity, Description: in.Description,
		OffendingTool: in.OffendingTool, ResourceID: in.ResourceID,
	})
}

// accumulateInternalIssue appends report to the per-turn accumulator, resetting
// it first if turnID differs from the accumulator's current turn (a stale,
// never-drained turn's reports are dropped rather than leaking into the next
// turn - mirrors the toolNames clear-on-turn-change behavior above). Safe to
// call concurrently with DrainInternalIssues.
func (t *Tailer) accumulateInternalIssue(turnID string, report turn.InternalIssueReport) {
	t.iiMu.Lock()
	defer t.iiMu.Unlock()
	if turnID != t.iiTurnID {
		t.internalIssues = nil
		t.iiTurnID = turnID
	}
	t.internalIssues = append(t.internalIssues, report)
}

// DrainInternalIssues returns and clears the accumulated internal-issue
// reports for turnID, or nil when turnID is empty or does not match the
// accumulator's current turn (nothing reported this turn, or already
// drained). Safe to call from a goroutine other than the one running Follow.
func (t *Tailer) DrainInternalIssues(turnID string) []turn.InternalIssueReport {
	t.iiMu.Lock()
	defer t.iiMu.Unlock()
	if turnID == "" || turnID != t.iiTurnID {
		return nil
	}
	out := t.internalIssues
	t.internalIssues = nil
	t.iiTurnID = ""
	return out
}

// caughtUpPollInterval is how often CaughtUpTo re-checks readPos while
// waiting. It is short relative to pollInterval so the wait resolves close
// to whenever Follow actually catches up, rather than adding up to a full
// extra poll cycle of latency on top.
const caughtUpPollInterval = 10 * time.Millisecond

// CaughtUpTo blocks until Follow has read at least target bytes from the
// transcript file, or timeout elapses; it returns whether it caught up.
// Callers take target from an os.Stat of the transcript at the turn
// boundary, so "caught up" means Follow has consumed everything that was on
// disk at that moment - closing the race where a turn-boundary drain runs
// before Follow's next poll-interval read has processed the turn's final
// line. A timeout is not an error: the caller proceeds with the drain
// regardless so a stuck tailer can never hang turn completion.
func (t *Tailer) CaughtUpTo(target int64, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if t.readPos.Load() >= target {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(caughtUpPollInterval)
	}
}

func (t *Tailer) fireActivity(turnID string, probeOriginated bool) {
	if t.onActivity != nil && turnID != "" {
		t.onActivity(turnID, probeOriginated)
	}
}

// classifyProbeEnqueue reports whether entry is the `enqueue` line the CLI
// wrote for the wrapper's own in-flight probe, stamping the probe's
// EnqueuedAt as a side effect.
//
// The match is deliberately as narrow as it can be made: an `enqueue`
// operation whose content is byte-equal (modulo surrounding whitespace) to the
// registered probe text. Nothing else in the transcript is ever excluded from
// the turn's inactivity heartbeat - in particular the removal line and the
// answer itself both count as activity, because by then the agent has demonstrably
// reached a tool-call boundary and is genuinely alive. The enqueue alone proves
// nothing: the CLI writes it the instant the paste lands, whether or not the
// agent is running.
func (t *Tailer) classifyProbeEnqueue(entry transcriptEntry) bool {
	if t.probes == nil || entry.Type != "queue-operation" || clampQueueOp(entry.Operation) != "enqueue" {
		return false
	}
	return t.probes.NoteEnqueue(queueContentText(entry.Content), entry.entryTime())
}

// knownNonMessageTypes is the fixed cardinality set for non-message transcript entries.
// `queue-operation`, `attachment` and `file-history-snapshot` are all common
// in real transcripts and were previously clamping to "other", which made the
// "other" bucket useless as an unknown-shape signal.
var knownNonMessageTypes = map[string]bool{
	"system": true, "summary": true, "user": true, "assistant": true,
	"queue-operation": true, "attachment": true, "file-history-snapshot": true,
}

// clampNonMessageType maps unknown entry.Type values to "other" so the
// ccw_stream_events_total metric label set stays bounded.
func clampNonMessageType(t string) string {
	if knownNonMessageTypes[t] {
		return t
	}
	return "other"
}

// Follow reads the transcript at path from the start, then follows appends
// until ctx is cancelled. Handles file not existing yet and inode changes.
func (t *Tailer) Follow(ctx context.Context, path string) error {
	var (
		f      *os.File
		reader *bufio.Reader
		inodeN uint64
	)

	openFile := func() error {
		if f != nil {
			_ = f.Close()
		}
		var err error
		f, err = os.Open(path)
		if err != nil {
			return err
		}
		fi, err := f.Stat()
		if err != nil {
			_ = f.Close()
			f = nil
			return err
		}
		inodeN = inode(fi)
		reader = bufio.NewReader(f)
		t.readPos.Store(0)
		return nil
	}

	// Wait for the file to exist
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := openFile(); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}

	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()

	var partial []byte

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			// readPos must count only bytes that have been fully handed to and
			// returned from processLine - never bytes merely read into a
			// buffer. Advancing it earlier would let CaughtUpTo (polled from
			// the drain goroutine) observe "caught up" for a line that has
			// been read off disk but not yet accumulated, dropping the
			// report a turn-boundary drain is waiting on.
			// Could be a partial line if err == io.EOF
			if err == nil {
				// Complete line: append partial if any, process it, then
				// advance readPos by the full processed length (any
				// previously-buffered partial bytes plus this line).
				full := append(partial, line...)
				partial = nil
				t.processLine(full)
				t.readPos.Add(int64(len(full)))
			} else {
				// Partial line at EOF - accumulate up to cap. Do NOT advance
				// readPos here: these bytes are buffered but unprocessed.
				partial = append(partial, line...)
				if len(partial) >= maxPartialBytes {
					t.processLine(partial)
					t.readPos.Add(int64(len(partial)))
					partial = nil
				}
			}
		}

		if err == io.EOF || err == nil {
			if err == nil {
				continue
			}
			// Check for inode change (file replaced)
			fi, statErr := os.Stat(path)
			if statErr == nil && inode(fi) != inodeN {
				// File replaced - reopen from start
				partial = nil
				if openErr := openFile(); openErr == nil {
					continue
				}
			}
			// Wait for more data
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(pollInterval):
			}
			continue
		}

		// Unexpected read error - log, reset partial, and retry rather than
		// permanently killing the tail. A single transient I/O error must not
		// end transcript streaming for the rest of the session.
		t.log.Warn("transcript read error, reopening",
			"action", "tailer_reopen",
			"err", err.Error(),
		)
		partial = nil
		_ = openFile()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// transcriptEntry is the top-level envelope of a single JSONL line.
type transcriptEntry struct {
	Type      string         `json:"type"`
	UUID      string         `json:"uuid"`
	SessionID string         `json:"sessionId"`
	Timestamp string         `json:"timestamp"`
	Message   *transcriptMsg `json:"message,omitempty"`

	// Operation and Content carry the `queue-operation` payload. Content is
	// raw because it is the queue key, and a `remove` line often omits it
	// entirely.
	Operation string          `json:"operation,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`

	// IsSidechain marks an entry belonging to a subagent's conversation
	// rather than the main one; ParentUUID links it back. Both are how a
	// subagent's work is identified whether it is inlined in the parent
	// transcript or written to subagents/agent-<id>.jsonl. ParentUUID is
	// often JSON null, which unmarshals to "".
	IsSidechain bool   `json:"isSidechain,omitempty"`
	ParentUUID  string `json:"parentUuid,omitempty"`
}

// entryTime parses the entry's RFC3339 timestamp, falling back to now when it
// is missing or unparseable.
func (e transcriptEntry) entryTime() time.Time {
	if e.Timestamp != "" {
		if ts, err := time.Parse(time.RFC3339, e.Timestamp); err == nil {
			return ts
		}
	}
	return time.Now()
}

type transcriptMsg struct {
	Role       string            `json:"role"`
	Content    []json.RawMessage `json:"content"`
	StopReason string            `json:"stop_reason,omitempty"`
	Usage      json.RawMessage   `json:"usage,omitempty"`
}

type contentBlock struct {
	Type string `json:"type"`
	// text / thinking
	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

// processQueueOperation logs, counts and tracks one `queue-operation` entry.
// The queue key is the raw JSON of the entry's content so an enqueue and its
// removal compare byte-for-byte; a removal with no content clears the whole
// pending set (see queueTracker.remove).
func (t *Tailer) processQueueOperation(entry transcriptEntry, turnID string) {
	op := clampQueueOp(entry.Operation)
	key := string(entry.Content)

	t.log.Info("agent stream queue operation",
		"action", "agent_stream_queue_operation",
		"stream_type", "queue-operation",
		"op", entry.Operation,
		"session_id", entry.SessionID,
		"transcript_uuid", entry.UUID,
		"ts", entry.Timestamp,
		"turn_id", turnID,
		"content", t.redactor.Scrub(key),
	)
	if t.queueOpsCounter != nil {
		t.queueOpsCounter.WithLabelValues(op).Inc() //nolint:errcheck
	}

	switch op {
	case "enqueue":
		t.queue.enqueue(key, entry.entryTime())
	case "remove", "dequeue":
		t.queue.remove(key)
		// A removal is what actually hands a queued message to the model, so
		// it is what promotes a pending probe to delivered. It is matched by
		// SHAPE, not by content: a real remove/dequeue line usually carries no
		// content at all, so there is nothing to compare against the probe
		// text. `dequeue` is an undocumented third operation that behaves like
		// `remove` and is far from rare (1266 vs 1156 on-disk), so ignoring it
		// would strand roughly half of all probes in pending forever.
		if t.probes != nil {
			t.probes.NoteRemoval(entry.entryTime())
		}
	}
}

func (t *Tailer) processLine(raw []byte) {
	// Trim trailing newline
	for len(raw) > 0 && (raw[len(raw)-1] == '\n' || raw[len(raw)-1] == '\r') {
		raw = raw[:len(raw)-1]
	}
	if len(raw) == 0 {
		return
	}

	// Capture turnID once per line so all branches share the same value and
	// the session mutex is taken at most once per poll cycle.
	turnID := t.turnID()

	// The line is unmarshalled BEFORE the activity hook fires because whether
	// it counts as agent progress depends on what it is: the enqueue line the
	// wrapper's own probe caused must not reset the deadline of the turn it is
	// probing. A malformed line still counts as activity (it came from the
	// agent's process either way) and is reported below.
	var entry transcriptEntry
	parseErr := json.Unmarshal(raw, &entry)

	// Any non-empty transcript line is agent progress: signal liveness for the
	// in-flight turn so the session can treat its deadline as an inactivity timer.
	probeOriginated := parseErr == nil && t.classifyProbeEnqueue(entry)
	t.fireActivity(turnID, probeOriginated)

	// On turn change, drop any tool_use ids left uncorrelated by the prior turn
	// (a tool_result that never arrived) so toolNames cannot grow unbounded.
	if t.toolCallsCounter != nil && turnID != t.tcTurnID {
		clear(t.toolNames)
		t.tcTurnID = turnID
	}

	// Same reasoning for outstanding Task calls: a subagent whose tool_result
	// never arrived cannot outlive the turn that launched it, so the counter
	// resets rather than drifting upward for the life of the pod.
	if turnID != t.taskTurnID {
		clear(t.taskUseIDs)
		t.taskCalls.Store(0)
		t.taskTurnID = turnID
	}

	if parseErr != nil {
		// Malformed line - emit raw event, never drop
		t.log.Info("agent stream",
			"action", "agent_stream",
			"stream_type", "raw",
			"raw_line", t.redactor.Scrub(string(raw)),
			"parse_error", parseErr.Error(),
			"turn_id", turnID,
		)
		t.incCounter("raw")
		return
	}

	// A sidechain entry is a subagent's work. Logged at DEBUG so the parent
	// link is queryable without adding a field to every ordinary line.
	if entry.IsSidechain {
		t.log.Debug("agent stream sidechain",
			"action", "agent_stream_sidechain",
			"stream_type", entry.Type,
			"session_id", entry.SessionID,
			"transcript_uuid", entry.UUID,
			"parent_uuid", entry.ParentUUID,
			"is_sidechain", true,
			"ts", entry.Timestamp,
			"turn_id", turnID,
		)
	}

	// queue-operation carries no message, so it must be handled BEFORE the
	// entry.Message == nil return below. It is how Claude Code records a
	// message parked while the turn is mid-tool-call: `enqueue` when the CLI
	// accepts it, `remove`/`dequeue` when it is actually handed to the model.
	// An enqueue with no matching removal therefore means the turn is wedged
	// inside one long tool call - the signal a later phase acts on. This phase
	// only records it.
	if entry.Type == "queue-operation" {
		t.processQueueOperation(entry, turnID)
	}

	if entry.Message == nil {
		// Non-message line: Claude Code transcript housekeeping (system, summary,
		// mode, permission-mode, file-history-snapshot, attachment, ai-title, ...)
		// carrying no agent/user content. Logged at DEBUG so the default INFO
		// stream shows only real messages and tool usage; the metric still counts
		// every entry. Clamp the metric label to a known set; use the raw type
		// only in the log (logs are not cardinality-bound).
		metricType := clampNonMessageType(entry.Type)
		t.log.Debug("agent stream",
			"action", "agent_stream",
			"stream_type", entry.Type,
			"session_id", entry.SessionID,
			"transcript_uuid", entry.UUID,
			"ts", entry.Timestamp,
			"turn_id", turnID,
		)
		t.incCounter(metricType)
		return
	}

	msg := entry.Message

	// Emit one event per content block
	for _, rawBlock := range msg.Content {
		var block contentBlock
		if err := json.Unmarshal(rawBlock, &block); err != nil {
			t.log.Info("agent stream",
				"action", "agent_stream",
				"stream_type", "raw",
				"session_id", entry.SessionID,
				"transcript_uuid", entry.UUID,
				"ts", entry.Timestamp,
				"turn_id", turnID,
				"raw_line", t.redactor.Scrub(string(rawBlock)),
				"parse_error", err.Error(),
			)
			t.incCounter("raw")
			continue
		}

		switch block.Type {
		case "text":
			t.log.Info("agent stream",
				"action", "agent_stream",
				"stream_type", "text",
				"session_id", entry.SessionID,
				"transcript_uuid", entry.UUID,
				"ts", entry.Timestamp,
				"turn_id", turnID,
				"role", msg.Role,
				"text", t.redactor.Scrub(block.Text),
			)
			t.incCounter("text")
			// A probe answer is an ordinary assistant text block - the spike
			// confirmed it lands in the transcript exactly like any other, at
			// the next tool-call boundary, without disturbing the turn in
			// flight. Only assistant blocks are considered so the CLI's echo
			// of the probe text back as a user message (which necessarily
			// contains the marker, since the probe asks for it) cannot answer
			// the probe itself.
			if t.probes != nil && msg.Role == "assistant" {
				if latency, ok := t.probes.NoteAssistantText(block.Text, entry.entryTime()); ok {
					t.log.Info("liveness probe answered",
						"action", "probe_answered",
						"turn_id", turnID,
						"session_id", entry.SessionID,
						"latency_ms", latency.Milliseconds(),
						"text", t.redactor.Scrub(block.Text),
					)
				}
			}
		case "thinking":
			if block.Thinking == "" {
				// Empty Thinking field is an unexpected shape - emit raw so it is
				// visible rather than silently coerced via the dead text fallback.
				t.log.Info("agent stream",
					"action", "agent_stream",
					"stream_type", "raw",
					"session_id", entry.SessionID,
					"transcript_uuid", entry.UUID,
					"ts", entry.Timestamp,
					"turn_id", turnID,
					"raw_line", t.redactor.Scrub(string(rawBlock)),
				)
				t.incCounter("raw")
				continue
			}
			t.log.Info("agent stream",
				"action", "agent_stream",
				"stream_type", "thinking",
				"session_id", entry.SessionID,
				"transcript_uuid", entry.UUID,
				"ts", entry.Timestamp,
				"turn_id", turnID,
				"text", t.redactor.Scrub(block.Thinking),
			)
			t.incCounter("thinking")
		case "tool_use":
			inputStr := t.redactor.Scrub(string(block.Input))
			t.log.Info("agent stream",
				"action", "agent_stream",
				"stream_type", "tool_use",
				"session_id", entry.SessionID,
				"transcript_uuid", entry.UUID,
				"ts", entry.Timestamp,
				"turn_id", turnID,
				"tool", block.Name,
				"tool_use_id", block.ID,
				"input", inputStr,
			)
			t.incCounter("tool_use")
			if block.Name == taskToolName && block.ID != "" {
				if _, dup := t.taskUseIDs[block.ID]; !dup {
					t.taskUseIDs[block.ID] = struct{}{}
					t.taskCalls.Add(1)
				}
			}
			if t.toolCallsCounter != nil {
				// Record id -> clamped name so the matching tool_result (which
				// carries only the id) can be attributed to a tool.
				t.toolNames[block.ID] = clampToolName(block.Name)
			}
			if block.Name == internalIssueToolName {
				t.emitInternalIssue(turnID, block.Input)
			}
		case "tool_result":
			contentStr := t.redactor.Scrub(string(block.Content))
			t.log.Info("agent stream",
				"action", "agent_stream",
				"stream_type", "tool_result",
				"session_id", entry.SessionID,
				"transcript_uuid", entry.UUID,
				"ts", entry.Timestamp,
				"turn_id", turnID,
				"tool_use_id", block.ToolUseID,
				"is_error", block.IsError,
				"content", contentStr,
			)
			t.incCounter("tool_result")
			if _, ok := t.taskUseIDs[block.ToolUseID]; ok {
				delete(t.taskUseIDs, block.ToolUseID)
				t.taskCalls.Add(-1)
			}
			if t.toolCallsCounter != nil {
				// Attribute the result to its tool via the correlation map; a
				// result with no matching tool_use clamps to "other".
				tool := "other"
				if name, ok := t.toolNames[block.ToolUseID]; ok {
					tool = name
					delete(t.toolNames, block.ToolUseID)
				}
				outcome := "success"
				if block.IsError {
					outcome = "error"
				}
				t.toolCallsCounter.WithLabelValues(tool, outcome).Inc() //nolint:errcheck
			}
		default:
			// Unknown block type - emit raw
			t.log.Info("agent stream",
				"action", "agent_stream",
				"stream_type", "raw",
				"session_id", entry.SessionID,
				"transcript_uuid", entry.UUID,
				"ts", entry.Timestamp,
				"turn_id", turnID,
				"raw_line", t.redactor.Scrub(string(rawBlock)),
			)
			t.incCounter("raw")
		}
	}

	// Emit message_end envelope event when there is a stop_reason
	if msg.StopReason != "" {
		t.log.Info("agent stream",
			"action", "agent_stream",
			"stream_type", "message_end",
			"session_id", entry.SessionID,
			"transcript_uuid", entry.UUID,
			"ts", entry.Timestamp,
			"turn_id", turnID,
			"role", msg.Role,
			"stop_reason", msg.StopReason,
			"usage", t.redactor.Scrub(string(msg.Usage)),
		)
		t.incCounter("message_end")
	}
}
