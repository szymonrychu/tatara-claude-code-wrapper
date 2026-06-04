package obs

import (
	"io"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
)

// NewLogger returns a JSON-format slog.Logger writing to w at the given level.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(h)
}

// RequestFields holds the structured fields attached to every HTTP request log entry.
type RequestFields struct {
	RequestID  string
	User       string
	Route      string
	Method     string
	Status     int
	DurationMs int64
}

// RequestLogger returns a child logger with standard HTTP request fields pre-attached.
func RequestLogger(base *slog.Logger, f RequestFields) *slog.Logger {
	return base.With(
		slog.String("request_id", f.RequestID),
		slog.String("user", f.User),
		slog.String("route", f.Route),
		slog.String("method", f.Method),
		slog.Int("status", f.Status),
		slog.Int64("duration_ms", f.DurationMs),
	)
}

// Obs bundles structured logging and metrics.
type Obs struct {
	Logger   *slog.Logger
	Registry *prometheus.Registry
}

// New constructs an Obs bundle from cfg, initialising logging and metrics.
func New(w io.Writer, level slog.Level) *Obs {
	return &Obs{
		Logger:   NewLogger(w, level),
		Registry: PromRegistry(),
	}
}
