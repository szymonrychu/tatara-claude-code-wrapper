// Command cc-statusline is the Claude Code statusLine command installed into
// every agent pod's settings.json. Claude Code passes it a JSON payload on
// stdin carrying rate_limits.{five_hour,seven_day}.{used_percentage,resets_at};
// this command forwards that snapshot to the wrapper over loopback and PRINTS
// NOTHING, because its stdout IS the rendered status line.
//
// It mirrors cmd/cc-stop-hook (own package main, stderr-only logging, always
// exit 0) with ONE deliberate difference: a single 500ms attempt, no retries.
// The Stop hook fires once per turn and its payload is irreplaceable, so it
// retries 5 times over 25s. The statusline fires on every TUI redraw and its
// payload is superseded seconds later, so retrying would only add latency to a
// redraw. A dropped snapshot costs nothing; a slow redraw costs the agent.
//
// The feed is SUBSCRIBER-ONLY: claude populates rate_limits only for a
// Claude.ai subscription credential, and only after the session's first API
// response. Agent pods authenticate with a CLAUDE_CODE_OAUTH_TOKEN from
// `claude setup-token`, so they qualify; a pod running on ANTHROPIC_API_KEY
// would never report, which is a silent no-op by design.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"
)

const postTimeout = 500 * time.Millisecond

// report is the wire body POSTed to the wrapper's loopback internal API. Its
// JSON tags must match internal/httpapi.accountUsageRequest exactly.
type report struct {
	ObservedAt        time.Time `json:"observedAt"`
	FiveHourPercent   float64   `json:"fiveHourPercent"`
	FiveHourResetUnix int64     `json:"fiveHourResetUnix,omitempty"`
	WeeklyPercent     float64   `json:"weeklyPercent"`
	WeeklyResetUnix   int64     `json:"weeklyResetUnix,omitempty"`
}

// window is one usage window as claude renders it. resets_at is UNIX EPOCH
// SECONDS, a JSON number - NOT an RFC3339 string. The operator's separate
// /api/oauth/usage client does speak RFC3339 for its own ResetsAt; the two
// sources genuinely disagree, and this is the statusline one. A string here
// fails the decode, which drops the report rather than coercing it to a zero
// reset that the gate would misread as a live window.
type window struct {
	UsedPercentage float64 `json:"used_percentage"`
	ResetsAt       int64   `json:"resets_at"`
}

// rateLimits carries the two windows as POINTERS because each is individually
// optional: claude omits either one, and the whole block is absent until the
// session's first API response.
type rateLimits struct {
	FiveHour *window `json:"five_hour"`
	SevenDay *window `json:"seven_day"`
}

type statusPayload struct {
	RateLimits *rateLimits `json:"rate_limits"`
}

// parsePayload extracts the account usage report from the statusline stdin
// payload. ok is false whenever the payload is unparseable, carries no
// rate_limits block (normal before the first API response, or on a non
// subscription credential), or carries a rate_limits block with neither
// window: the caller then reports NOTHING rather than reporting zeros, which
// would read to the operator as "0% used" and wrongly hold the gate open.
func parsePayload(b []byte, now func() time.Time) (report, bool) {
	var p statusPayload
	if err := json.Unmarshal(b, &p); err != nil || p.RateLimits == nil {
		return report{}, false
	}
	if p.RateLimits.FiveHour == nil && p.RateLimits.SevenDay == nil {
		return report{}, false
	}
	rep := report{ObservedAt: now().UTC()}
	if w := p.RateLimits.FiveHour; w != nil {
		rep.FiveHourPercent = w.UsedPercentage
		rep.FiveHourResetUnix = w.ResetsAt
	}
	if w := p.RateLimits.SevenDay; w != nil {
		rep.WeeklyPercent = w.UsedPercentage
		rep.WeeklyResetUnix = w.ResetsAt
	}
	return rep, true
}

func run(stdin io.Reader, stdout io.Writer, url string, now func() time.Time) error {
	_ = stdout // the status line stays empty; the parameter exists so tests can assert that
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	rep, ok := parsePayload(raw, now)
	if !ok {
		return nil
	}
	body, err := json.Marshal(rep)
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), postTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("post account usage: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("post account usage: unexpected status %d", resp.StatusCode)
	}
	return nil
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	url := envOr("CCW_STATUSLINE_URL", "http://127.0.0.1:8090/internal/account-usage")
	if err := run(os.Stdin, os.Stdout, url, time.Now); err != nil {
		log.Warn("cc-statusline report failed", "action", "statusline_post", "err", err)
	}
	os.Exit(0) // never block or alter claude
}

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}
