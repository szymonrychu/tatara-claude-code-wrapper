// Package webhook delivers turn results to caller-supplied callback URLs.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

// maxBackoff caps exponential backoff growth to prevent int64 overflow and
// bound the worst-case retry rate regardless of the configured Retries value.
const maxBackoff = 60 * time.Second

type Config struct {
	Retries int
	Backoff time.Duration
}

type Sender struct {
	cfg    Config
	client *http.Client
	m      *metrics.Metrics
	log    *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	closed bool
}

func New(cfg Config, m *metrics.Metrics, log *slog.Logger) *Sender {
	if cfg.Backoff <= 0 {
		cfg.Backoff = time.Second
	}
	ctx, cancel := context.WithCancel(context.Background()) //nolint:gosec // cancel is stored in s.cancel and invoked by Shutdown
	return &Sender{cfg: cfg, client: &http.Client{Timeout: 30 * time.Second}, m: m, log: log, ctx: ctx, cancel: cancel}
}

// Deliver posts the record to url asynchronously, retrying with exponential
// backoff. A blank url is a no-op (poll-only callers). Delivery runs under the
// sender's own context and is tracked so Shutdown can drain or abort it.
// Calls received after Shutdown has begun are silently dropped so that wg.Add
// cannot race with wg.Wait.
func (s *Sender) Deliver(url string, rec *turn.Record) {
	if url == "" {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.log.Warn("webhook: deliver after shutdown, dropping", "turn_id", rec.ID, "url", url)
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()

	body, err := json.Marshal(rec)
	if err != nil {
		s.wg.Done()
		s.log.Error("webhook: marshal", "err", err, "turn_id", rec.ID)
		return
	}
	go func() {
		defer s.wg.Done()
		s.deliver(s.ctx, url, rec.ID, body)
	}()
}

// Shutdown drains in-flight deliveries. It waits for tracked goroutines to
// finish their retries cleanly until ctx's deadline; if that bounded drain
// window elapses first, it cancels in-flight retries (which log a clean abort
// and record a "dropped" outcome) and joins the goroutines before returning.
// After Shutdown returns, subsequent Deliver calls are silently dropped.
func (s *Sender) Shutdown(ctx context.Context) {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		s.cancel()
		<-done
	}
	s.cancel() // release the context regardless of which branch won
}

func (s *Sender) deliver(ctx context.Context, url, turnID string, body []byte) {
	backoff := s.cfg.Backoff
	for attempt := 0; attempt <= s.cfg.Retries; attempt++ {
		if err := s.post(ctx, url, body); err != nil {
			s.log.Warn("webhook: attempt failed", "err", err, "turn_id", turnID, "attempt", attempt)
			select {
			case <-ctx.Done():
				s.m.WebhookDelivery.WithLabelValues("dropped").Inc()
				s.log.Warn("webhook: aborted on shutdown", "turn_id", turnID, "url", url, "attempt", attempt)
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		s.m.WebhookDelivery.WithLabelValues("ok").Inc()
		s.log.Info("webhook: delivered", "turn_id", turnID, "url", url)
		return
	}
	s.m.WebhookDelivery.WithLabelValues("dropped").Inc()
	s.log.Error("webhook: dropped after retries", "turn_id", turnID, "url", url)
}

func (s *Sender) post(ctx context.Context, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
