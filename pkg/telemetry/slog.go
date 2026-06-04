package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// traceHandler wraps an slog.Handler and injects trace_id and span_id
// attributes from the span context when a recording span is active.
type traceHandler struct {
	inner slog.Handler
}

// newTraceHandler wraps inner with trace context injection.
func newTraceHandler(inner slog.Handler) slog.Handler {
	return &traceHandler{inner: inner}
}

// Enabled delegates to the inner handler.
func (h *traceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle adds trace_id and span_id attributes if a recording span is active,
// then delegates to the inner handler.
func (h *traceHandler) Handle(ctx context.Context, record slog.Record) error {
	sc := trace.SpanFromContext(ctx).SpanContext()
	if sc.HasTraceID() {
		record.AddAttrs(slog.String("trace_id", sc.TraceID().String()))
	}
	if sc.HasSpanID() {
		record.AddAttrs(slog.String("span_id", sc.SpanID().String()))
	}
	return h.inner.Handle(ctx, record)
}

// WithAttrs returns a new handler with the given attributes.
func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{inner: h.inner.WithAttrs(attrs)}
}

// WithGroup returns a new handler with the given group name.
func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{inner: h.inner.WithGroup(name)}
}
