package storage

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// telemetryInstr holds shared OTel instruments for storage providers.
// Both localProvider and s3Provider embed this struct. All fields are
// nil-safe — methods are safe to call when telemetry is not configured.
type telemetryInstr struct {
	tracer    trace.Tracer
	meter     metric.Meter
	opCounter metric.Int64Counter
	opLatency metric.Int64Histogram
}

// startSpan creates a tracing span. If no tracer is configured, falls back to
// the tracer from the parent span's TracerProvider.
func (t *telemetryInstr) startSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	if t.tracer != nil {
		return t.tracer.Start(ctx, name)
	}
	return trace.SpanFromContext(ctx).TracerProvider().Tracer("storage").Start(ctx, name)
}

// recordSuccess records a successful operation: increments the operation
// counter, records latency, and sets the span status to OK.
func (t *telemetryInstr) recordSuccess(ctx context.Context, span trace.Span, start time.Time) {
	if t.opCounter != nil {
		t.opCounter.Add(ctx, 1)
	}
	if t.opLatency != nil {
		t.opLatency.Record(ctx, time.Since(start).Milliseconds())
	}
	span.SetStatus(codes.Ok, "")
}

// initMetrics creates the standard storage metric instruments on the given
// meter. Returns an error if instrument creation fails.
func (t *telemetryInstr) initMetrics(meter metric.Meter) error {
	t.meter = meter
	var err error
	t.opCounter, err = meter.Int64Counter("storage.operations.total",
		metric.WithDescription("Total storage operations"))
	if err != nil {
		return fmt.Errorf("create operation counter: %w", err)
	}
	t.opLatency, err = meter.Int64Histogram("storage.operation.duration_ms",
		metric.WithDescription("Storage operation duration in milliseconds"))
	if err != nil {
		return fmt.Errorf("create operation latency histogram: %w", err)
	}
	return nil
}
