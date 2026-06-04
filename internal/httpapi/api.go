// Package httpapi exposes the wrapper's public (OIDC) and internal (loopback)
// HTTP surfaces.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/auth"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/session"
	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/turn"
)

// SessionController is the slice of session.Manager the API needs.
type SessionController interface {
	Submit(text, callbackURL string) (string, error)
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
}

type API struct {
	ctl   SessionController
	store *turn.Store
	v     *auth.Verifier
	log   *slog.Logger
	reg   *prometheus.Registry
}

func New(d Deps) *API {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return &API{ctl: d.Ctl, store: d.Store, v: d.Verifier, log: d.Log, reg: d.Registry}
}

// Router is the public surface: OIDC-gated /v1/* plus open operator endpoints.
func (a *API) Router() http.Handler {
	r := chi.NewRouter()
	r.Group(func(pr chi.Router) {
		if a.v != nil {
			pr.Use(auth.Middleware(a.v))
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
