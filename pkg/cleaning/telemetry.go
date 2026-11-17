package cleaning

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// engineTelemetry holds the engine's instruments; each is nil when not
// configured, and every use checks.
type engineTelemetry struct {
	tracer   trace.Tracer
	findings metric.Int64Counter
	duration metric.Float64Histogram
	pending  metric.Int64Gauge
}

// WithEngineTelemetry instruments the engine: a span per public method with
// tenant.id and cleaning.operation attributes, cleaning.findings.total
// (findings an apply wrote, by signal and action),
// cleaning.operation.duration_ms (by operation and outcome) and
// cleaning.findings.pending (the tenant's review queue, set after an apply or
// a decision). There is no config field: the caller passes the providers it
// instruments the rest of the daemon with. A nil provider leaves that half
// uninstrumented; an instrument the meter refuses is logged and left out.
func WithEngineTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) EngineOption {
	return func(e *Engine) {
		if tp != nil {
			e.tel.tracer = tp.Tracer("crosscodex/pkg/cleaning")
		}
		if mp == nil {
			return
		}
		meter := mp.Meter("crosscodex")
		var err error
		if e.tel.findings, err = meter.Int64Counter("cleaning.findings.total",
			metric.WithDescription("Findings a cleaning apply wrote, by signal and action")); err != nil {
			e.tel.findings = nil
			slog.Warn("cleaning: cleaning.findings.total is not recorded", "error", err)
		}
		if e.tel.duration, err = meter.Float64Histogram("cleaning.operation.duration_ms",
			metric.WithDescription("Cleaning engine operation duration"), metric.WithUnit("ms")); err != nil {
			e.tel.duration = nil
			slog.Warn("cleaning: cleaning.operation.duration_ms is not recorded", "error", err)
		}
		if e.tel.pending, err = meter.Int64Gauge("cleaning.findings.pending",
			metric.WithDescription("Findings in the tenant's review queue")); err != nil {
			e.tel.pending = nil
			slog.Warn("cleaning: cleaning.findings.pending is not recorded", "error", err)
		}
	}
}

// begin opens the span of one public method and checks that ctx carries
// tenantID, so no graph or store read happens for another tenant. The
// returned finish records the outcome and duration and ends the span; when
// the tenant check fails, begin finishes the operation itself.
func (e *Engine) begin(ctx context.Context, method, op, tenantID string) (context.Context, func(error), error) {
	begun := time.Now()
	ctx, span := telemetry.StartSpan(e.tel.tracer, ctx, "cleaning."+method)
	span.SetAttributes(attribute.String("tenant.id", tenantID), attribute.String("cleaning.operation", op))
	finish := func(err error) {
		outcome := "ok"
		if err != nil {
			outcome = "error"
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		if e.tel.duration != nil {
			e.tel.duration.Record(ctx, float64(time.Since(begun))/float64(time.Millisecond),
				metric.WithAttributes(attribute.String("operation", op), attribute.String("outcome", outcome)))
		}
		span.End()
	}
	ctxTenant, err := tenant.FromContext(ctx)
	switch {
	case err != nil:
		err = fmt.Errorf("cleaning %s: the request carries no authenticated tenant, so it was refused and nothing was read or written. "+
			"Call it through the gateway, which scopes every request to the caller's tenant: %w", op, err)
	case ctxTenant != tenantID:
		// Neither tenant is named: the message must not reveal another tenant.
		err = fmt.Errorf("cleaning %s: %w", op, ErrTenantMismatch)
	}
	if err != nil {
		finish(err)
		return ctx, nil, err
	}
	return ctx, finish, nil
}

// recordPending sets cleaning.findings.pending from the store. It runs after
// a write has succeeded, so a failed count is logged rather than returned: the
// caller's write must not be reported as failed.
func (e *Engine) recordPending(ctx context.Context, tenantID string) {
	if e.tel.pending == nil {
		return
	}
	n, err := e.store.PendingCount(ctx, tenantID)
	if err != nil {
		slog.WarnContext(ctx, "cleaning: count pending findings for the gauge", "tenant.id", tenantID, "error", err)
		return
	}
	e.tel.pending.Record(ctx, n, metric.WithAttributes(attribute.String("tenant.id", tenantID)))
}
