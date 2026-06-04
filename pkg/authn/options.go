package authn

import (
	"fmt"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// RegistryOption configures a Registry.
type RegistryOption func(*Registry) error

// WithTelemetry configures OpenTelemetry tracing and metrics for the registry.
func WithTelemetry(tracer trace.Tracer, meter metric.Meter) RegistryOption {
	return func(r *Registry) error {
		r.tracer = tracer
		r.meter = meter

		var err error
		r.authCounter, err = meter.Int64Counter("authn.attempts.total",
			metric.WithDescription("Total authentication attempts"))
		if err != nil {
			return fmt.Errorf("create auth counter: %w", err)
		}
		r.authLatency, err = meter.Int64Histogram("authn.duration_ms",
			metric.WithDescription("Authentication duration in milliseconds"))
		if err != nil {
			return fmt.Errorf("create auth latency histogram: %w", err)
		}
		return nil
	}
}
