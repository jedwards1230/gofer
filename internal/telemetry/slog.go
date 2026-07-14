package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// logHandler wraps an inner [slog.Handler], stamping trace_id/span_id
// attributes onto a record when its ctx carries an active, sampled span.
type logHandler struct {
	inner slog.Handler
}

// NewLogHandler wraps inner so that a log call made with a ctx carrying an
// active, sampled span gets trace_id and span_id string attributes added
// before the record reaches inner. A ctx with no active span (or an
// unsampled one) passes through unchanged.
//
// Correlation only appears when the log call's own ctx carries a span — the
// instrumenter's turn/model/tool spans live in their own per-session
// goroutine ctx (see [Provider.Instrument]), so broad propagation into every
// daemon log site is future work; this ships the handler and proves it
// works (see slog_test.go).
func NewLogHandler(inner slog.Handler) slog.Handler {
	return &logHandler{inner: inner}
}

// Enabled delegates to inner.
func (h *logHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle adds trace_id/span_id (when ctx carries an active, sampled span)
// and delegates to inner.
func (h *logHandler) Handle(ctx context.Context, rec slog.Record) error {
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() && sc.IsSampled() {
		rec.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.inner.Handle(ctx, rec)
}

// WithAttrs wraps the inner handler's WithAttrs result, preserving
// correlation on the returned handler.
func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logHandler{inner: h.inner.WithAttrs(attrs)}
}

// WithGroup wraps the inner handler's WithGroup result, preserving
// correlation on the returned handler.
func (h *logHandler) WithGroup(name string) slog.Handler {
	return &logHandler{inner: h.inner.WithGroup(name)}
}
