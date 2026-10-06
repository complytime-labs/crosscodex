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

// WithMaxBulkEdges sets the most edges one BulkCreateEdges request may carry.
// Wired from config graph.max_bulk_edges (config.GraphConfig.MaxBulkEdges),
// whose validation keeps n in [1, 10000]. Options cannot return errors, so a
// value below 1 is ignored and the service keeps its default,
// config.DefaultGraphMaxBulkEdges, rather than rejecting every request.
func WithMaxBulkEdges(n int) Option {
	return func(s *Service) {
		if n >= 1 {
			s.maxBulkEdges = n
		}
	}
}

// WithLogger sets the structured logger. A nil logger is ignored, keeping
// slog.Default().
func WithLogger(logger *slog.Logger) Option {
	return func(s *Service) {
		if logger != nil {
			s.logger = logger
		}
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
