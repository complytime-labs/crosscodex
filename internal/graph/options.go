package graph

import (
	"log/slog"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Option configures a Service.
type Option func(*Service)

// WithTelemetry enables OpenTelemetry tracing and metrics.
func WithTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) Option {
	return func(s *Service) {
		if tp != nil {
			s.tracer = tp.Tracer("crosscodex/internal/graph")
		}
		if mp != nil {
			m := mp.Meter("crosscodex")
			s.rpcCounter, _ = m.Int64Counter("graph.rpc.total")
			s.rpcLatency, _ = m.Float64Histogram("graph.rpc.duration_ms")
			s.eventCounter, _ = m.Int64Counter("graph.events.total")
			s.materializeLatency, _ = m.Float64Histogram("graph.materialize.duration_ms")
		}
	}
}

// WithLogger sets the structured logger.
func WithLogger(logger *slog.Logger) Option {
	return func(s *Service) {
		s.logger = logger
	}
}

// WithResolver registers a ResourceResolver for its scheme.
// NOTE: ResolverRegistry is implemented in Task 3 (resolver.go).
func WithResolver(r ResourceResolver) Option {
	return func(s *Service) {
		s.resolvers.Register(r)
	}
}

// WithResolverRegistry sets a custom ResolverRegistry.
// Used in tests to inject a pre-configured registry.
func WithResolverRegistry(registry *ResolverRegistry) Option {
	return func(s *Service) {
		s.resolvers = registry
	}
}
