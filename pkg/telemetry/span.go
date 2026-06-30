package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/trace"
)

// StartSpan creates a tracing span if the tracer is non-nil, otherwise returns
// the context unchanged and a non-recording span from the context. This is a
// convenience function for packages that accept an optional trace.Tracer.
func StartSpan(tracer trace.Tracer, ctx context.Context, name string) (context.Context, trace.Span) {
	if tracer == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	return tracer.Start(ctx, name)
}
