package pipeline

import (
	"log/slog"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Option configures a Service.
type Option func(*Service)

// WithTelemetry enables OTel tracing and metrics for the pipeline Service.
func WithTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) Option {
	return func(s *Service) {
		s.tracerProvider = tp
		s.meterProvider = mp
		if tp != nil {
			s.tracer = tp.Tracer("crosscodex/internal/pipeline")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			s.jobCounter, _ = meter.Int64Counter("pipeline.jobs.total",
				metric.WithDescription("Total pipeline jobs by status"))
			s.jobDuration, _ = meter.Float64Histogram("pipeline.job.duration_ms",
				metric.WithDescription("Pipeline job duration"))
		}
	}
}

// WithLogger sets the Service's structured logger.
func WithLogger(logger *slog.Logger) Option {
	return func(s *Service) {
		if logger != nil {
			s.logger = logger
		}
	}
}
