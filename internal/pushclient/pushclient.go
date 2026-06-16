// Package pushclient pushes the wrapper's Prometheus metrics to the operator's
// push-receiver. The wrapper Pod is too short-lived to be reliably pull-scraped
// (a scrape can miss a pod that has already exited), so it pushes its /metrics
// to tatara-operator, which aggregates and re-exposes them for normal scraping.
//
// Each run is keyed by a unique run_id (plus pod and job labels) so concurrent
// and successive runs never clobber each other's series. On graceful shutdown
// the client best-effort DELETEs its series; the operator-side TTL is the
// backstop for a hard-killed pod.
package pushclient

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
)

// Config holds the inputs the operator injects into the wrapper Pod env. Push
// is disabled (a no-op) unless both URL and RunID are set.
type Config struct {
	URL      string        // operator push endpoint, e.g. http://tatara-operator-internal.tatara.svc:8082/internal/metrics/push
	RunID    string        // unique per run; keys this run's series
	Pod      string        // pod name label
	Job      string        // job label; defaults to "tatara-claude-code-wrapper"
	Interval time.Duration // push period; defaults to 15s
}

// Pusher periodically pushes gathered metrics to the operator and removes them
// on shutdown.
type Pusher struct {
	cfg    Config
	g      prometheus.Gatherer
	m      *metrics.Metrics
	client *http.Client
	log    *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New builds a Pusher gathering from g. It applies defaults for Job and
// Interval; call Enabled to check whether pushing is configured.
func New(cfg Config, g prometheus.Gatherer, log *slog.Logger, m ...*metrics.Metrics) *Pusher {
	if cfg.Job == "" {
		cfg.Job = "tatara-claude-code-wrapper"
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 15 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pusher{
		cfg:    cfg,
		g:      g,
		client: &http.Client{Timeout: 10 * time.Second},
		log:    log,
		ctx:    ctx,
		cancel: cancel,
	}
	if len(m) > 0 {
		p.m = m[0]
	}
	return p
}

// Enabled reports whether the operator wired a push URL and run_id.
func (p *Pusher) Enabled() bool { return p.cfg.URL != "" && p.cfg.RunID != "" }

// Start launches the push loop. It is a no-op when push is not configured.
func (p *Pusher) Start() {
	if !p.Enabled() {
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.loop()
	}()
}

func (p *Pusher) loop() {
	t := time.NewTicker(p.cfg.Interval)
	defer t.Stop()
	p.pushOnce(p.ctx) // push immediately so a fast-exiting run is not lost
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-t.C:
			p.pushOnce(p.ctx)
		}
	}
}

// Shutdown stops the loop and best-effort deletes this run's series so they are
// gone immediately rather than waiting for the operator TTL.
func (p *Pusher) Shutdown(ctx context.Context) {
	if !p.Enabled() {
		return
	}
	p.cancel()
	p.wg.Wait()
	p.delete(ctx)
}

func (p *Pusher) pushOnce(ctx context.Context) {
	body, err := encode(p.g)
	if err != nil {
		p.log.Warn("pushclient: gather/encode failed", "err", err)
		if p.m != nil {
			p.m.MetricPushTotal.WithLabelValues("encode_fail").Inc()
		}
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint(), bytes.NewReader(body))
	if err != nil {
		p.log.Warn("pushclient: build request failed", "err", err)
		if p.m != nil {
			p.m.MetricPushTotal.WithLabelValues("encode_fail").Inc()
		}
		return
	}
	req.Header.Set("Content-Type", string(expfmt.NewFormat(expfmt.TypeTextPlain)))
	resp, err := p.client.Do(req)
	if err != nil {
		p.log.Warn("pushclient: push failed", "err", err, "run_id", p.cfg.RunID)
		if p.m != nil {
			p.m.MetricPushTotal.WithLabelValues("transport_fail").Inc()
		}
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		p.log.Warn("pushclient: push rejected", "status", resp.StatusCode, "run_id", p.cfg.RunID)
		if p.m != nil {
			p.m.MetricPushTotal.WithLabelValues("rejected").Inc()
		}
		return
	}
	if p.m != nil {
		p.m.MetricPushTotal.WithLabelValues("ok").Inc()
	}
}

func (p *Pusher) delete(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, p.endpoint(), nil)
	if err != nil {
		return
	}
	resp, err := p.client.Do(req)
	if err != nil {
		p.log.Warn("pushclient: delete failed", "err", err, "run_id", p.cfg.RunID)
		return
	}
	_ = resp.Body.Close()
}

// endpoint returns the push URL with the identity query parameters appended.
func (p *Pusher) endpoint() string {
	q := url.Values{}
	q.Set("run_id", p.cfg.RunID)
	if p.cfg.Pod != "" {
		q.Set("pod", p.cfg.Pod)
	}
	q.Set("job", p.cfg.Job)
	sep := "?"
	if bytes.ContainsRune([]byte(p.cfg.URL), '?') {
		sep = "&"
	}
	return p.cfg.URL + sep + q.Encode()
}

// encode gathers from g and renders the families in Prometheus text format,
// which the operator's push-receiver parses.
func encode(g prometheus.Gatherer) ([]byte, error) {
	mfs, err := g.Gather()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			return nil, fmt.Errorf("encode %s: %w", mf.GetName(), err)
		}
	}
	return buf.Bytes(), nil
}
