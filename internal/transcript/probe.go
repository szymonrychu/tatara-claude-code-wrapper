package transcript

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// ProbeAnswerPrefix is the literal an assistant text block must start with to
// count as an answer to a liveness probe. It is a marker rather than a
// heuristic on purpose: the probe asks the agent to reply with exactly this
// token followed by one sentence, so matching it is unambiguous and cannot be
// satisfied by the agent's ordinary output.
const ProbeAnswerPrefix = "TATARA-ALIVE"

// ProbeState is the lifecycle of one liveness probe, derived entirely from
// transcript lines.
//
// The three states are NOT a success ladder with one failure mode at the end -
// each carries a distinct diagnosis, and collapsing them loses the whole point
// of probing:
//
//   - ProbeStatePending: the CLI wrote the `enqueue` line (or has not even got
//     that far) and no removal has followed. The message was accepted but never
//     handed to the model, which means the turn has not reached a tool-call
//     boundary since the probe was sent - i.e. it is blocked INSIDE a single
//     long tool call. Verified: a 70s sleep buffered a probe for 58.2s while
//     its enqueue line appeared immediately. This is the highest-signal
//     diagnosis available, not a probe failure.
//   - ProbeStateDelivered: a removal line followed, so the model has the
//     message. The agent is reaching tool-call boundaries. If it never answers
//     from here, the model is running but not responsive to steering.
//   - ProbeStateAnswered: an assistant text block starting with
//     ProbeAnswerPrefix arrived after delivery. The agent is alive and
//     steerable; the original turn resumes normally afterwards.
type ProbeState string

const (
	ProbeStatePending   ProbeState = "pending"
	ProbeStateDelivered ProbeState = "delivered"
	ProbeStateAnswered  ProbeState = "answered"
)

// Terminal probe outcomes, the label values of ccw_probe_outcomes_total. They
// map 1:1 onto the states above at the moment a probe stops being observed.
const (
	ProbeOutcomeAnswered            = "answered"
	ProbeOutcomeDeliveredUnanswered = "delivered_unanswered"
	ProbeOutcomeNeverDelivered      = "never_delivered"
)

// ProbeStatus is the externally visible view of one probe.
type ProbeStatus struct {
	ID    string     `json:"probeId"`
	State ProbeState `json:"state"`
	// SentAt is when the wrapper finished writing the probe to the PTY.
	SentAt time.Time `json:"sentAt"`
	// EnqueuedAt is the timestamp of the `queue-operation`/`enqueue` line the
	// CLI wrote for this probe. Zero while the tailer has not seen it yet (or
	// when transcript tailing is disabled), which is why it is reported
	// separately from State: state pending + zero EnqueuedAt means "written,
	// not yet observed", while state pending + a set EnqueuedAt is the
	// blocked-inside-one-tool-call diagnosis.
	EnqueuedAt time.Time `json:"enqueuedAt,omitzero"`
	// DeliveredAt is the timestamp of the removal line that handed the probe to
	// the model.
	DeliveredAt time.Time `json:"deliveredAt,omitzero"`
	// AnsweredAt is the timestamp of the assistant text block that answered.
	AnsweredAt time.Time `json:"answeredAt,omitzero"`
	// Answer is that block's text, which is what the operator reads to decide
	// whether the agent is doing something useful.
	Answer string `json:"answer,omitzero"`
}

// probeRecord is the tracker's mutable state for one probe.
type probeRecord struct {
	status   ProbeStatus
	text     string
	finished bool // a terminal outcome has already been emitted
}

// ProbeTracker follows one in-flight liveness probe through the transcript.
//
// It is owned by the session Manager rather than the Tailer so that a probe
// can still be registered and reported when transcript tailing is disabled
// (CCW_LOG_TRANSCRIPT=false); the Tailer is handed a reference and reports
// observations into it.
//
// Only ONE probe is tracked at a time: registering a new one finalises its
// predecessor. The operator's escalation ladder sends probes strictly in
// sequence, and keeping a history would be an unbounded map keyed by a value
// the wrapper never garbage-collects.
type ProbeTracker struct {
	mu        sync.Mutex
	cur       *probeRecord
	onOutcome func(outcome string, answerLatency time.Duration)
}

func NewProbeTracker() *ProbeTracker { return &ProbeTracker{} }

// WithOutcomeObserver attaches the callback invoked exactly once per probe
// when it reaches a terminal outcome: immediately on an answer, or at
// finalisation (a superseding probe, or Finalize at shutdown) otherwise.
// answerLatency is meaningful only for ProbeOutcomeAnswered.
func (p *ProbeTracker) WithOutcomeObserver(fn func(outcome string, answerLatency time.Duration)) *ProbeTracker {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onOutcome = fn
	return p
}

// Register begins tracking a probe, finalising whatever came before it.
func (p *ProbeTracker) Register(id, text string, at time.Time) {
	p.mu.Lock()
	emit := p.finishLocked()
	p.cur = &probeRecord{
		status: ProbeStatus{ID: id, State: ProbeStatePending, SentAt: at},
		text:   strings.TrimSpace(text),
	}
	fn := p.onOutcome
	p.mu.Unlock()
	emit(fn)
}

// Discard drops the tracked probe without emitting an outcome. Used when the
// PTY write failed: ccw_probes_total{result="write_fail"} already records that,
// and counting a never_delivered outcome on top would double-report a probe
// that was never actually sent.
func (p *ProbeTracker) Discard(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cur != nil && p.cur.status.ID == id {
		p.cur = nil
	}
}

// Finalize closes out the in-flight probe, emitting its terminal outcome.
// Idempotent.
func (p *ProbeTracker) Finalize() {
	p.mu.Lock()
	emit := p.finishLocked()
	fn := p.onOutcome
	p.mu.Unlock()
	emit(fn)
}

// finishLocked marks the current probe terminal and returns a closure that
// emits its outcome. The emission is deferred out of the critical section so
// the observer (which increments prometheus collectors) never runs under the
// tracker's mutex.
func (p *ProbeTracker) finishLocked() func(func(string, time.Duration)) {
	cur := p.cur
	if cur == nil || cur.finished {
		return func(func(string, time.Duration)) {}
	}
	cur.finished = true
	outcome := ProbeOutcomeNeverDelivered
	var latency time.Duration
	switch cur.status.State {
	case ProbeStateAnswered:
		outcome = ProbeOutcomeAnswered
		latency = cur.status.AnsweredAt.Sub(cur.status.SentAt)
	case ProbeStateDelivered:
		outcome = ProbeOutcomeDeliveredUnanswered
	case ProbeStatePending:
		outcome = ProbeOutcomeNeverDelivered
	}
	return func(fn func(string, time.Duration)) {
		if fn != nil {
			fn(outcome, latency)
		}
	}
}

// Status returns the tracked probe when id matches it.
func (p *ProbeTracker) Status(id string) (ProbeStatus, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cur == nil || p.cur.status.ID != id {
		return ProbeStatus{}, false
	}
	return p.cur.status, true
}

// Current returns the tracked probe, whatever its id.
func (p *ProbeTracker) Current() (ProbeStatus, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cur == nil {
		return ProbeStatus{}, false
	}
	return p.cur.status, true
}

// PendingSince returns the send time of a probe that has not been delivered to
// the model, and whether there is one. It is the value published as
// Snapshot.ProbePendingSince: a stamp that keeps growing older is the
// blocked-inside-one-tool-call signal.
func (p *ProbeTracker) PendingSince() (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cur == nil || p.cur.status.State != ProbeStatePending {
		return time.Time{}, false
	}
	return p.cur.status.SentAt, true
}

// NoteEnqueue reports a `queue-operation`/`enqueue` line to the tracker and
// returns true when its content is the registered probe's own text.
//
// The boolean is the ONLY way the probe's own lines are identified, and it is
// deliberately restricted to the enqueue side. A real `remove`/`dequeue` line
// usually carries no content at all (verified on-disk: 2469 enqueue / 1156
// remove / 1266 dequeue), so a tracker that also demanded content equality on
// the removal side would never match one and would report a permanent false
// stall for every probe.
func (p *ProbeTracker) NoteEnqueue(content string, at time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.cur
	if cur == nil || cur.text == "" || strings.TrimSpace(content) != cur.text {
		return false
	}
	if cur.status.EnqueuedAt.IsZero() {
		cur.status.EnqueuedAt = at
	}
	return true
}

// NoteRemoval reports a `remove`/`dequeue` line, which is what hands a queued
// message to the model. It carries no content to match on, so any removal seen
// while a probe is pending transitions that probe to delivered. That is sound
// because the wrapper is the only writer to this PTY: nothing else can put a
// message in the queue behind the probe's back.
func (p *ProbeTracker) NoteRemoval(at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.cur
	if cur == nil || cur.status.State != ProbeStatePending {
		return
	}
	cur.status.State = ProbeStateDelivered
	cur.status.DeliveredAt = at
}

// NoteAssistantText reports an assistant text block. It answers the probe when
// the probe has been delivered, the block is not older than the delivery, and
// the text starts with ProbeAnswerPrefix. Returns the answer latency and true
// when this call is what transitioned the probe to answered.
func (p *ProbeTracker) NoteAssistantText(text string, at time.Time) (time.Duration, bool) {
	p.mu.Lock()
	cur := p.cur
	if cur == nil || cur.status.State != ProbeStateDelivered {
		p.mu.Unlock()
		return 0, false
	}
	if at.Before(cur.status.DeliveredAt) {
		p.mu.Unlock()
		return 0, false
	}
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, ProbeAnswerPrefix) {
		p.mu.Unlock()
		return 0, false
	}
	cur.status.State = ProbeStateAnswered
	cur.status.AnsweredAt = at
	cur.status.Answer = trimmed
	emit := p.finishLocked()
	fn := p.onOutcome
	latency := at.Sub(cur.status.SentAt)
	p.mu.Unlock()
	emit(fn)
	return latency, true
}

// queueContentText renders a `queue-operation` entry's `content` payload as
// plain text. It is normally a JSON string; any other shape falls back to the
// raw JSON, which simply will not match a probe's text (the safe direction: a
// shape change costs the activity exclusion, not correctness of the queue
// tracking).
func queueContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}
