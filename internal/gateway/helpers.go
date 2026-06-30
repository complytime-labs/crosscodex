package gateway

import (
	"context"

	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// startHandlerSpan creates a tracing span with standard gateway handler
// attributes (rpc.method, tenant.id, user.id) plus any extra attributes.
// Returns the original context and a noop function if the tracer is nil.
// Callers must defer the returned function to end the span.
func (s *Service) startHandlerSpan(ctx context.Context, method string, identity *authn.Identity, extra ...attribute.KeyValue) (context.Context, func()) {
	if s.tracer == nil {
		return ctx, func() {}
	}
	attrs := make([]attribute.KeyValue, 0, 3+len(extra))
	attrs = append(attrs,
		attribute.String("rpc.method", method),
		attribute.String("tenant.id", identity.TenantID),
		attribute.String("user.id", identity.Subject),
	)
	attrs = append(attrs, extra...)
	ctx, span := s.tracer.Start(ctx, "gateway."+method, trace.WithAttributes(attrs...))
	return ctx, func() { span.End() }
}

// emitAttestation creates an in-toto attestation link. On error the failure
// is recorded to the active span but does not fail the request.
func (s *Service) emitAttestation(ctx context.Context, step string, materials, products []attestation.Artifact, byProducts map[string]any) {
	if s.attestor == nil {
		return
	}
	traceID := telemetry.TraceIDFromContext(ctx)
	link, err := s.attestor.CreateLink(ctx, step, materials, products, attestation.WithByProducts(byProducts))
	if err != nil {
		if s.tracer != nil {
			trace.SpanFromContext(ctx).RecordError(err, trace.WithAttributes(
				attribute.String("error.type", "attestation_failed"),
				attribute.String("trace_id", traceID),
			))
		}
	} else {
		_ = link
	}
}
