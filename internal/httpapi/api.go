// Package httpapi exposes the wrapper's public (OIDC) and internal (loopback)
// HTTP surfaces.
package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/auth"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/obs"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

// SessionController is the slice of session.Manager the API needs.
type SessionController interface {
	Submit(text, callbackURL string, handoff bool) (string, error)
	Complete(session.HookResult) error
	Snapshot() session.Snapshot
	TranscriptPath() string
	Alive() bool
	Shutdown(context.Context) error
}

type Deps struct {
	Ctl      SessionController
	Store    *turn.Store
	Verifier *auth.Verifier
	Log      *slog.Logger
	Registry *prometheus.Registry
	Metrics  *metrics.Metrics
}

type API struct {
	ctl   SessionController
	store *turn.Store
	v     *auth.Verifier
	log   *slog.Logger
	reg   *prometheus.Registry
	m     *metrics.Metrics
}

func New(d Deps) *API {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return &API{ctl: d.Ctl, store: d.Store, v: d.Verifier, log: d.Log, reg: d.Registry, m: d.Metrics}
}

// probeRoutes are the infra-plane endpoints whose access logs are pure noise at
// INFO: kubelet hits /healthz + /readyz every ~10s and Prometheus scrapes
// /metrics on its own interval. Their "request handled" line is demoted to DEBUG
// so the default-level access log shows only real requests; set LOG_LEVEL=debug
// to see them again.
var probeRoutes = map[string]bool{
	"/healthz": true,
	"/readyz":  true,
	"/metrics": true,
}

// requestLogger is a chi middleware that logs each request on completion with
// request_id, user (from OIDC claims if present), route, method, status, and
// duration_ms. Business requests log at INFO (rules 12+13); infra-plane probe
// routes log at DEBUG (see probeRoutes).
func (a *API) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		user := ""
		if claims, ok := auth.ClaimsFromContext(r.Context()); ok {
			user = claims.Subject
		}
		lg := obs.RequestLogger(a.log, obs.RequestFields{
			RequestID:  middleware.GetReqID(r.Context()),
			User:       user,
			Route:      r.URL.Path,
			Method:     r.Method,
			Status:     ww.Status(),
			DurationMs: time.Since(start).Milliseconds(),
		})
		if probeRoutes[r.URL.Path] {
			lg.Debug("request handled")
		} else {
			lg.Info("request handled")
		}
	})
}

// httpMetrics returns the HTTP metrics middleware stack (in-flight gauge,
// request counter + latency histogram, panic recovery counter).
func (a *API) httpMetrics() func(http.Handler) http.Handler {
	if a.m == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					a.m.HTTPPanicsTotal.Inc()
					a.log.ErrorContext(r.Context(), "http handler panic recovered",
						"action", "http_panic",
						"request_id", middleware.GetReqID(r.Context()),
						"route", r.URL.Path, "method", r.Method,
						"panic", fmt.Sprintf("%v", rec))
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()
			a.m.HTTPInFlight.Inc()
			defer a.m.HTTPInFlight.Dec()
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			route := chi.RouteContext(r.Context()).RoutePattern()
			if route == "" {
				route = r.URL.Path
			}
			status := fmt.Sprintf("%d", ww.Status())
			a.m.HTTPRequestsTotal.WithLabelValues(route, r.Method, status).Inc()
			a.m.HTTPRequestDuration.WithLabelValues(route).Observe(time.Since(start).Seconds())
		})
	}
}

// Router is the public surface: OIDC-gated /v1/* plus open operator endpoints.
func (a *API) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(a.httpMetrics())
	r.Use(a.requestLogger)
	r.Group(func(pr chi.Router) {
		if a.v != nil {
			if a.m != nil {
				pr.Use(auth.Middleware(a.v, a.m.AuthTotal))
			} else {
				pr.Use(auth.Middleware(a.v))
			}
		}
		a.mountV1(pr)
	})
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.Get("/readyz", a.readyz)
	if a.reg != nil {
		r.Handle("/metrics", promhttp.HandlerFor(a.reg, promhttp.HandlerOpts{}))
	}
	return r
}

// TestRouter is the public surface without OIDC, for handler unit tests.
func (a *API) TestRouter() http.Handler {
	r := chi.NewRouter()
	a.mountV1(r)
	r.Get("/readyz", a.readyz)
	return r
}

func (a *API) mountV1(r chi.Router) {
	r.Post("/v1/messages", a.postMessage)
	r.Get("/v1/messages", a.listMessages)
	r.Get("/v1/messages/{turnID}", a.getMessage)
	r.Get("/v1/session", a.getSession)
	r.Get("/v1/transcript", a.getTranscript)
	r.Delete("/v1/session", a.deleteSession)
}

// InternalRouter is the loopback-only surface the Stop hook posts to.
func (a *API) InternalRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(a.requestLogger)
	r.Post("/internal/turn-complete", a.turnComplete)
	return r
}

func (a *API) readyz(w http.ResponseWriter, _ *http.Request) {
	if a.ctl.Alive() {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Error(w, "not ready", http.StatusServiceUnavailable)
}
