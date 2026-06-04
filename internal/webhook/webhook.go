// Package webhook delivers turn results to caller-supplied callback URLs.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

type Config struct {
	Retries int
	Backoff time.Duration
}

type Sender struct {
	cfg    Config
	client *http.Client
	m      *metrics.Metrics
	log    *slog.Logger
}

func New(cfg Config, m *metrics.Metrics, log *slog.Logger) *Sender {
	if cfg.Backoff <= 0 {
		cfg.Backoff = time.Second
	}
	return &Sender{cfg: cfg, client: &http.Client{Timeout: 30 * time.Second}, m: m, log: log}
}

// Deliver posts the record to url asynchronously, retrying with exponential
// backoff. A blank url is a no-op (poll-only callers).
func (s *Sender) Deliver(ctx context.Context, url string, rec *turn.Record) {
	if url == "" {
		return
	}
	body, err := json.Marshal(rec)
	if err != nil {
		s.log.Error("webhook: marshal", "err", err, "turn_id", rec.ID)
		return
	}
	go s.deliver(ctx, url, rec.ID, body)
}

func (s *Sender) deliver(ctx context.Context, url, turnID string, body []byte) {
	backoff := s.cfg.Backoff
	for attempt := 0; attempt <= s.cfg.Retries; attempt++ {
		if err := s.post(ctx, url, body); err != nil {
			s.log.Warn("webhook: attempt failed", "err", err, "turn_id", turnID, "attempt", attempt)
			select {
			case <-ctx.Done():
				s.m.WebhookDelivery.WithLabelValues("dropped").Inc()
				return
			case <-time.After(backoff):
			}
			backoff *= 2
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
