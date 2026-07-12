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
	onActivity           func(turnID string)

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
	return &Tailer{log: log, redactor: redactor, turnID: turnID}
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

// WithActivity attaches a hook fired once per processed transcript line that
// carries an in-flight turn id. It is the per-turn liveness heartbeat the
// session uses to reset its inactivity deadline. Returns self for chaining.
func (t *Tailer) WithActivity(fn func(turnID string)) *Tailer {
	t.onActivity = fn
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

func (t *Tailer) fireActivity(turnID string) {
	if t.onActivity != nil && turnID != "" {
		t.onActivity(turnID)
	}
}

// knownNonMessageTypes is the fixed cardinality set for non-message transcript entries.
var knownNonMessageTypes = map[string]bool{
	"system": true, "summary": true, "user": true, "assistant": true,
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

	// Any non-empty transcript line is agent progress: signal liveness for the
	// in-flight turn so the session can treat its deadline as an inactivity timer.
	t.fireActivity(turnID)

	// On turn change, drop any tool_use ids left uncorrelated by the prior turn
	// (a tool_result that never arrived) so toolNames cannot grow unbounded.
	if t.toolCallsCounter != nil && turnID != t.tcTurnID {
		clear(t.toolNames)
		t.tcTurnID = turnID
	}

	var entry transcriptEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		// Malformed line - emit raw event, never drop
		t.log.Info("agent stream",
			"action", "agent_stream",
			"stream_type", "raw",
			"raw_line", t.redactor.Scrub(string(raw)),
			"parse_error", err.Error(),
			"turn_id", turnID,
		)
		t.incCounter("raw")
		return
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
