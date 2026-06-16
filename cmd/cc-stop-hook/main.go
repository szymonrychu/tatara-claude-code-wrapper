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

const (
	maxPostAttempts = 5
	postPerAttempt  = 5 * time.Second
	postBackoff     = 500 * time.Millisecond
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("cc-stop-hook failed", "action", "hook_post", "err", err)
	}
	os.Exit(0) // never block or alter claude
}

func run(log *slog.Logger) error {
	payload, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	internalURL := envOr("CCW_INTERNAL_URL", "http://127.0.0.1:8090/internal/turn-complete")
	resultPath := envOr("CCW_RESULT_JSON", "/workspace/result.json")

	res, err := buildResult(payload, resultPath)
	if err != nil {
		return err
	}
	body, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}
	start := time.Now()
	postErr := postWithRetry(internalURL, body)
	durationMs := time.Since(start).Milliseconds()
	if postErr != nil {
		log.Error("hook post exhausted all attempts",
			"action", "hook_post",
			"session_id", res.SessionID,
			"attempts", maxPostAttempts,
			"duration_ms", durationMs,
			"err", postErr)
		return postErr
	}
	log.Info("hook post succeeded",
		"action", "hook_post",
		"session_id", res.SessionID,
		"duration_ms", durationMs)
	return nil
}

// postWithRetry POSTs body to url, retrying up to maxPostAttempts times on
// network errors or non-2xx responses.  Each attempt has its own per-request
// timeout so a hung connection never blocks the full budget.
func postWithRetry(url string, body []byte) error {
	var lastErr error
	for attempt := 0; attempt < maxPostAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(postBackoff)
		}
		ctx, cancel := context.WithTimeout(context.Background(), postPerAttempt)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			cancel()
			return fmt.Errorf("new request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err != nil {
			lastErr = fmt.Errorf("post result (attempt %d): %w", attempt+1, err)
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("post result (attempt %d): unexpected status %d", attempt+1, resp.StatusCode)
			continue
		}
		return nil
	}
	return lastErr
}

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}
