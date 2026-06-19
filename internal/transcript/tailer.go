package transcript

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const pollInterval = 200 * time.Millisecond

// maxPartialBytes caps the in-memory accumulator for partial (non-newline-
// terminated) lines. Matches the 16 MiB scanner limit used by the stop-hook
// reader in transcript.go. Exceeding this emits a raw event and resets
// partial so a pathological write cannot grow memory unbounded.
const maxPartialBytes = 16 * 1024 * 1024

// StreamCounter is satisfied by *prometheus.CounterVec.
type StreamCounter interface {
	WithLabelValues(lvs ...string) prometheus.Counter
}

// Tailer reads a JSONL transcript file from the start and follows appends.
// It re-opens the file on inode change (claude restart) and never drops a
// malformed line (emits a raw event instead).
type Tailer struct {
	log        *slog.Logger
	redactor   *Redactor
	turnID     func() string
	counter    StreamCounter
	onActivity func(turnID string)
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
			// Could be a partial line if err == io.EOF
			if err == nil {
				// Complete line: append partial if any
				full := append(partial, line...)
				partial = nil
				t.processLine(full)
			} else {
				// Partial line at EOF - accumulate up to cap
				partial = append(partial, line...)
				if len(partial) >= maxPartialBytes {
					t.processLine(partial)
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
		// Non-message line (system, summary, etc.) - passthrough.
		// Clamp the metric label to a known set; use the raw type only in the log
		// (logs are not cardinality-bound).
		metricType := clampNonMessageType(entry.Type)
		t.log.Info("agent stream",
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
