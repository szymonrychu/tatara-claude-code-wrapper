package webhook_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/webhook"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newSender(t *testing.T, retries int) *webhook.Sender {
	t.Helper()
	return webhook.New(webhook.Config{Retries: retries, Backoff: time.Millisecond},
		metrics.New(prometheus.NewRegistry()), discardLogger())
}

func TestDeliver_SucceedsAfterRetry(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := newSender(t, 3)
	rec := &turn.Record{ID: "t1", State: turn.Complete, FinalText: "PONG"}
	s.Deliver(srv.URL, rec)
	require.Eventually(t, func() bool { return atomic.LoadInt32(&hits) == 2 }, time.Second, 5*time.Millisecond)
}

func TestDeliver_EmptyURLIsNoop(t *testing.T) {
	s := newSender(t, 1)
	s.Deliver("", &turn.Record{ID: "t1"}) // must not panic
}

func TestShutdown_AbortsInFlightRetries(t *testing.T) {
	// Server always fails, so the retry loop would otherwise back off forever.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := webhook.New(webhook.Config{Retries: 1_000_000, Backoff: 50 * time.Millisecond},
		metrics.New(prometheus.NewRegistry()), discardLogger())
	s.Deliver(srv.URL, &turn.Record{ID: "t1", State: turn.Complete})

	// A drain window that elapses immediately must cancel the goroutine and join.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { s.Shutdown(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return; in-flight delivery was orphaned")
	}
}

// TestBackoff_IsCapped verifies that repeated failures never produce a backoff
// that overflows time.Duration. With a huge Retries and a tiny initial backoff
// the deliver goroutine must still be abortable quickly after Shutdown.
func TestBackoff_IsCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	// 200 retries starting at 1 ms would overflow without a cap (2^63 ns ~ 63 doublings).
	s := webhook.New(webhook.Config{Retries: 200, Backoff: time.Millisecond},
		metrics.New(prometheus.NewRegistry()), discardLogger())
	s.Deliver(srv.URL, &turn.Record{ID: "backoff-cap", State: turn.Complete})

	// Cancel immediately; if backoff overflows and goes negative, time.After fires
	// at once and the loop becomes a tight spin that never yields to the select.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { s.Shutdown(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown hung: backoff likely overflowed and became a tight spin")
	}
}

// TestDeliver_AfterShutdown_DoesNotPanic verifies that calling Deliver after
// Shutdown has completed neither panics nor spawns an untracked goroutine.
func TestDeliver_AfterShutdown_DoesNotPanic(t *testing.T) {
	s := newSender(t, 0)
	// Shut down with an already-done context so it completes immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Shutdown(ctx)

	// This must not panic ("sync: WaitGroup is reused before previous Wait returned").
	require.NotPanics(t, func() {
		s.Deliver("http://127.0.0.1:0/no-such-server", &turn.Record{ID: "post-shutdown"})
	})
}

// TestDeliver_NoSleepAfterLastAttempt verifies that the retry loop does NOT
// sleep after the final failed attempt. With Retries=2 and Backoff=200ms the
// exponential sequence is 200ms, 400ms (i.e. sleeps between attempts 0->1 and
// 1->2). After attempt 2 fails the goroutine must record "dropped" immediately;
// it must NOT sleep an additional 800ms. We give 1200ms total budget - enough
// for the two legitimate inter-attempt sleeps but not the spurious post-last one.
func TestDeliver_NoSleepAfterLastAttempt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	const (
		retries = 2
		backoff = 200 * time.Millisecond
		// legitimate inter-attempt sleeps: 200ms + 400ms = 600ms
		// spurious post-last sleep would add another 800ms
		budget = 1400 * time.Millisecond
	)

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	s := webhook.New(webhook.Config{Retries: retries, Backoff: backoff}, m, discardLogger())

	start := time.Now()
	s.Deliver(srv.URL, &turn.Record{ID: "last-attempt-sleep", State: turn.Complete})

	// Wait for the "dropped" counter to appear within the budget.
	dropped := func() bool {
		mfs, _ := reg.Gather()
		for _, mf := range mfs {
			if mf.GetName() != "ccw_webhook_delivery_total" {
				continue
			}
			for _, metric := range mf.GetMetric() {
				for _, lp := range metric.GetLabel() {
					if lp.GetName() == "result" && lp.GetValue() == "dropped" {
						return metric.GetCounter().GetValue() >= 1
					}
				}
			}
		}
		return false
	}
	require.Eventually(t, dropped, budget, 10*time.Millisecond,
		"dropped metric not recorded within %v (elapsed %v); spurious post-last-attempt sleep suspected",
		budget, time.Since(start))
}

// TestDeliver_MarshalFailureCountedAsDropped verifies that a marshal error in
// Deliver increments ccw_webhook_delivery_total{result=dropped} (audit finding 6).
// We trigger a marshal error by using a turn.Record with an un-marshalable field.
// Since turn.Record is a plain struct that always marshals fine, we inject an
// unmarshalable ResultJSON directly.
func TestDeliver_MarshalFailureCountedAsDropped(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	s := webhook.New(webhook.Config{Retries: 0, Backoff: time.Millisecond}, m, discardLogger())

	// json.RawMessage with invalid JSON causes json.Marshal(rec) to fail.
	rec := &turn.Record{
		ID:         "bad-marshal",
		ResultJSON: json.RawMessage([]byte(`{invalid`)),
	}
	s.Deliver("http://127.0.0.1:0/irrelevant", rec)

	// Give the goroutine a moment to run (Deliver is async for the marshal step).
	// Actually the marshal happens before the goroutine is spawned, so it's synchronous.
	// But we need the wg to be released; require.Eventually for robustness.
	require.Eventually(t, func() bool {
		mfs, _ := reg.Gather()
		for _, mf := range mfs {
			if mf.GetName() != "ccw_webhook_delivery_total" {
				continue
			}
			for _, metric := range mf.GetMetric() {
				for _, lp := range metric.GetLabel() {
					if lp.GetName() == "result" && lp.GetValue() == "dropped" {
						return metric.GetCounter().GetValue() >= 1
					}
				}
			}
		}
		return false
	}, time.Second, 5*time.Millisecond, "ccw_webhook_delivery_total{result=dropped} not incremented on marshal error")
}

// operatorValidHMACSignature is an INDEPENDENT, inline re-implementation of
// tatara-operator's internal/controller.validHMACSignature (turncallback.go),
// copied by hand from the operator source (no shared Go module between the two
// repos). It exists only to prove, in this test, that the wrapper's signer and
// the operator's verifier agree byte-for-byte: sig must be
// "sha256=" + hex(HMAC-SHA256(body, secret)).
func operatorValidHMACSignature(body []byte, sig, secret string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(sig, prefix) {
		return false
	}
	got, err := hex.DecodeString(sig[len(prefix):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := mac.Sum(nil)
	return hmac.Equal(got, expected)
}

// TestPost_SignsBodyWithHMACWhenSecretConfigured is the F7 regression test:
// the wrapper must sign every callback POST with X-Tatara-Signature when a
// secret is configured, in the exact format tatara-operator's
// validHMACSignature verifies. It proves agreement by running the operator's
// verification algorithm (replicated above) against what the wrapper actually
// sent, rather than just asserting a header is present.
func TestPost_SignsBodyWithHMACWhenSecretConfigured(t *testing.T) {
	const secret = "test-callback-hmac-secret" //nolint:gosec // test fixture, not a real credential

	var (
		gotSig  string
		gotBody []byte
	)
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-Tatara-Signature")
		gotBody, _ = io.ReadAll(r.Body)
		close(done)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	s := webhook.New(webhook.Config{Retries: 0, Backoff: time.Millisecond, Secret: secret},
		metrics.New(prometheus.NewRegistry()), discardLogger())
	s.Deliver(srv.URL, &turn.Record{ID: "hmac-1", State: turn.Complete, FinalText: "ok"})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("callback never delivered")
	}

	require.NotEmpty(t, gotSig, "X-Tatara-Signature must be set when a secret is configured")
	require.True(t, operatorValidHMACSignature(gotBody, gotSig, secret),
		"signature %q over body %s must verify against the operator's algorithm with secret %q", gotSig, gotBody, secret)
}

// TestPost_NoSignatureHeaderWhenSecretEmpty verifies the backward-compatible
// no-op: deployments that have not configured CALLBACK_HMAC_SECRET must not
// suddenly start sending a (bogus) signature header.
func TestPost_NoSignatureHeaderWhenSecretEmpty(t *testing.T) {
	var gotSig string
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-Tatara-Signature")
		close(done)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	s := webhook.New(webhook.Config{Retries: 0, Backoff: time.Millisecond},
		metrics.New(prometheus.NewRegistry()), discardLogger())
	s.Deliver(srv.URL, &turn.Record{ID: "no-hmac-1", State: turn.Complete})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("callback never delivered")
	}
	require.Empty(t, gotSig)
}
