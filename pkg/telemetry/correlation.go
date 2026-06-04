package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/trace"
)

// TraceIDFromContext extracts the hex-encoded trace ID from the span in ctx.
// Returns an empty string if no valid trace ID is present.
func TraceIDFromContext(ctx context.Context) string {
	sc := trace.SpanFromContext(ctx).SpanContext()
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

// SpanIDFromContext extracts the hex-encoded span ID from the span in ctx.
// Returns an empty string if no valid span ID is present.
func SpanIDFromContext(ctx context.Context) string {
	sc := trace.SpanFromContext(ctx).SpanContext()
	if !sc.HasSpanID() {
		return ""
	}
	return sc.SpanID().String()
}
