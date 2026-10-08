package backup

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// instruments holds the package's spans and metrics (docs/dev/telemetry.md).
type instruments struct {
	tracer         trace.Tracer
	operations     metric.Int64Counter     // backup.operations.total{operation,result}
	stepDuration   metric.Float64Histogram // backup.step.duration_ms{store}
	bytes          metric.Int64Counter     // backup.bytes.total{store}
	verifyFailures metric.Int64Counter     // backup.verify.failures.total
	staleness      metric.Float64Gauge     // backup.staleness_seconds{store}
}

func newInstruments(tracer trace.Tracer, meter metric.Meter) (*instruments, error) {
	in := &instruments{tracer: tracer}
	var err error
	if in.operations, err = meter.Int64Counter("backup.operations.total",
		metric.WithDescription("Backup commands by operation and result")); err != nil {
		return nil, fmt.Errorf("create backup.operations.total: %w", err)
	}
	if in.stepDuration, err = meter.Float64Histogram("backup.step.duration_ms",
		metric.WithDescription("Duration of each store's capture in milliseconds")); err != nil {
		return nil, fmt.Errorf("create backup.step.duration_ms: %w", err)
	}
	if in.bytes, err = meter.Int64Counter("backup.bytes.total",
		metric.WithDescription("Bytes captured per store")); err != nil {
		return nil, fmt.Errorf("create backup.bytes.total: %w", err)
	}
	if in.verifyFailures, err = meter.Int64Counter("backup.verify.failures.total",
		metric.WithDescription("Integrity problems found by backup verify")); err != nil {
		return nil, fmt.Errorf("create backup.verify.failures.total: %w", err)
	}
	if in.staleness, err = meter.Float64Gauge("backup.staleness_seconds",
		metric.WithDescription("Age of each store's newest backup in seconds")); err != nil {
		return nil, fmt.Errorf("create backup.staleness_seconds: %w", err)
	}
	return in, nil
}

func endSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// op starts a command span; the returned func ends it and counts the result.
func (in *instruments) op(ctx context.Context, operation string) (context.Context, func(error)) {
	ctx, span := in.tracer.Start(ctx, "backup."+operation, trace.WithAttributes(attribute.String("backup.operation", operation)))
	return ctx, func(err error) {
		result := "ok"
		if err != nil {
			result = "error"
		}
		in.operations.Add(ctx, 1, metric.WithAttributes(attribute.String("operation", operation), attribute.String("result", result)))
		endSpan(span, err)
	}
}

// step starts a capture span for one store; the returned func records its
// duration and, on success, the bytes captured.
func (in *instruments) step(ctx context.Context, store string) (context.Context, func(int64, error)) {
	start := time.Now()
	ctx, span := in.tracer.Start(ctx, "backup.step."+store, trace.WithAttributes(attribute.String("backup.store", store)))
	return ctx, func(n int64, err error) {
		attrs := metric.WithAttributes(attribute.String("store", store))
		in.stepDuration.Record(ctx, float64(time.Since(start))/float64(time.Millisecond), attrs)
		if err == nil {
			in.bytes.Add(ctx, n, attrs)
		}
		endSpan(span, err)
	}
}

// span starts a plain span (restore steps).
func (in *instruments) span(ctx context.Context, name string) (context.Context, func(error)) {
	ctx, span := in.tracer.Start(ctx, name)
	return ctx, func(err error) { endSpan(span, err) }
}

func (in *instruments) recordVerify(ctx context.Context, rep VerifyReport) {
	var failures int64
	for _, p := range rep.Points {
		failures += int64(len(p.Issues))
	}
	for _, c := range rep.WAL {
		if c.Status == WALStatusFailure {
			failures++
		}
	}
	if failures > 0 {
		in.verifyFailures.Add(ctx, failures)
	}
	for _, s := range rep.Staleness {
		if s.NewestPoint != "" {
			in.staleness.Record(ctx, s.Age.Seconds(), metric.WithAttributes(attribute.String("store", s.Store)))
		}
	}
}
