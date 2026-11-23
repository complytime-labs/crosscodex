package feedback

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/telemetry"
)

// serviceTelemetry holds the service's instruments; each is nil when not
// configured, and every use checks.
type serviceTelemetry struct {
	tracer      trace.Tracer
	votes       metric.Int64Counter
	duration    metric.Float64Histogram
	unprojected metric.Int64Gauge
}

// WithTelemetry instruments the service: a span per public method named
// feedback.<Method> with tenant.id and feedback.operation attributes,
// feedback.operation.duration_ms (by operation and outcome),
// feedback.votes.total (votes recorded, by vote_type) and
// feedback.reviews.unprojected (the tenant's determinations not yet applied
// to the graph, set after each projection). There is no config field: the
// caller passes the providers it instruments the rest of the daemon with. A
// nil provider leaves that half uninstrumented; an instrument the meter
// refuses is logged and left out.
func WithTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) Option {
	return func(s *Service) {
		if tp != nil {
			s.tel.tracer = tp.Tracer("crosscodex/internal/feedback")
		}
		if mp == nil {
			return
		}
		meter := mp.Meter("crosscodex")
		var err error
		if s.tel.votes, err = meter.Int64Counter("feedback.votes.total",
			metric.WithDescription("Mapping votes recorded, by vote type")); err != nil {
			s.tel.votes = nil
			slog.Warn("feedback: feedback.votes.total is not recorded", "error", err)
		}
		if s.tel.duration, err = meter.Float64Histogram("feedback.operation.duration_ms",
			metric.WithDescription("Feedback service operation duration"), metric.WithUnit("ms")); err != nil {
			s.tel.duration = nil
			slog.Warn("feedback: feedback.operation.duration_ms is not recorded", "error", err)
		}
		if s.tel.unprojected, err = meter.Int64Gauge("feedback.reviews.unprojected",
			metric.WithDescription("Mapping determinations recorded but not yet applied to the graph")); err != nil {
			s.tel.unprojected = nil
			slog.Warn("feedback: feedback.reviews.unprojected is not recorded", "error", err)
		}
	}
}

// begin opens the span of one public method. The returned finish records
// the outcome and duration and ends the span. The tenant attribute is set
// by setTenant once the tenant is known.
func (s *Service) begin(ctx context.Context, method, op string) (context.Context, func(error)) {
	begun := time.Now()
	ctx, span := telemetry.StartSpan(s.tel.tracer, ctx, "feedback."+method)
	span.SetAttributes(attribute.String("feedback.operation", op))
	return ctx, func(err error) {
		outcome := "ok"
		if err != nil {
			outcome = "error"
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		if s.tel.duration != nil {
			s.tel.duration.Record(ctx, float64(time.Since(begun))/float64(time.Millisecond),
				metric.WithAttributes(attribute.String("operation", op), attribute.String("outcome", outcome)))
		}
		span.End()
	}
}

// setTenant adds the tenant.id attribute to the active span.
func setTenant(ctx context.Context, tenantID string) {
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("tenant.id", tenantID))
}
