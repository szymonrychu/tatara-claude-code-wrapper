// Package metricspush ships the wrapper's own gathered prometheus metrics to
// the operator's push-receiver. A wrapper pod is short-lived, so pull scraping
// can miss it entirely; pushing keeps a run observable while it lasts. Series
// are keyed by run_id/pod/job using the Prometheus Pushgateway grouping path so
// the operator never collides concurrent runs. On clean shutdown the run's
// series are deleted so the operator drops them immediately; the operator's TTL
// is the backstop for a hard kill where shutdown never runs. Every call is
// best-effort: a failed push or delete is logged at WARN and never blocks or
// fails the turn. Auth mirrors the turn-complete callback channel (the
// operator's internal endpoint is in-cluster only and takes no bearer token).
package metricspush

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
)

type Config struct {
	URL      string        // operator push-receiver base; blank disables pushing
	Interval time.Duration // periodic push cadence
	RunID    string        // unique per wrapper run
	Pod      string        // pod name (downward API / hostname)
	Job      string        // job label
	Timeout  time.Duration // per-request timeout
}

type Pusher struct {
	cfg      Config
	gather   prometheus.Gatherer
	m        *metrics.Metrics
	client   *http.Client
	log      *slog.Logger
	groupURL string

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func New(cfg Config, gather prometheus.Gatherer, m *metrics.Metrics, log *slog.Logger) *Pusher {
	if cfg.Interval <= 0 {
		cfg.Interval = 15 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Job == "" {
		cfg.Job = "tatara-claude-code-wrapper"
	}
	return &Pusher{
		cfg:      cfg,
		gather:   gather,
		m:        m,
		client:   &http.Client{Timeout: cfg.Timeout},
		log:      log,
		groupURL: groupingURL(cfg.URL, cfg.Job, cfg.RunID, cfg.Pod),
	}
}

// groupingURL builds the Pushgateway-style grouping path the operator keys on:
// <base>/metrics/job/<job>/run_id/<run_id>/pod/<pod>. Each label value becomes
// a label on every pushed series.
func groupingURL(base, job, runID, pod string) string {
	base = strings.TrimSuffix(base, "/")
	return base + "/metrics" +
		"/job/" + url.PathEscape(job) +
		"/run_id/" + url.PathEscape(runID) +
		"/pod/" + url.PathEscape(pod)
}

// Start launches the periodic push loop. A blank URL disables pushing. The loop
// runs until ctx is cancelled or Shutdown is called.
func (p *Pusher) Start(ctx context.Context) {
	if p.cfg.URL == "" {
		return
	}
	ctx, p.cancel = context.WithCancel(ctx)
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		t := time.NewTicker(p.cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.push(ctx)
			}
		}
	}()
}

// Shutdown stops the periodic loop, captures a final push so the last state is
// recorded, then best-effort deletes the run's series so the operator drops
// them immediately. All steps are best-effort and bounded by ctx. A blank URL
// is a no-op.
func (p *Pusher) Shutdown(ctx context.Context) {
	if p.cfg.URL == "" {
		return
	}
	p.once.Do(func() {
		if p.cancel != nil {
			p.cancel()
			<-p.done
		}
		p.push(ctx)
		p.delete(ctx)
	})
}

func (p *Pusher) push(ctx context.Context) {
	body, contentType, err := p.encode()
	if err != nil {
		p.record("push", "error")
		p.log.Warn("metricspush: gather failed", "err", err)
		return
	}
	if err := p.send(ctx, http.MethodPut, contentType, body); err != nil {
		p.record("push", "error")
		p.log.Warn("metricspush: push failed", "err", err, "url", p.groupURL)
		return
	}
	p.record("push", "ok")
}

func (p *Pusher) delete(ctx context.Context) {
	if err := p.send(ctx, http.MethodDelete, "", nil); err != nil {
		p.record("delete", "error")
		p.log.Warn("metricspush: delete failed", "err", err, "url", p.groupURL)
		return
	}
	p.record("delete", "ok")
}

func (p *Pusher) send(ctx context.Context, method, contentType string, body []byte) error {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.groupURL, r)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func (p *Pusher) encode() ([]byte, string, error) {
	mfs, err := p.gather.Gather()
	if err != nil {
		return nil, "", fmt.Errorf("gather: %w", err)
	}
	format := expfmt.NewFormat(expfmt.TypeTextPlain)
	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, format)
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			return nil, "", fmt.Errorf("encode %s: %w", mf.GetName(), err)
		}
	}
	return buf.Bytes(), string(format), nil
}

func (p *Pusher) record(op, result string) {
	if p.m != nil {
		p.m.MetricsPush.WithLabelValues(op, result).Inc()
	}
}
