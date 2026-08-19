package pipeline

import (
	"log/slog"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/config"
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

// WithCatalogControlsReader wires a CatalogControlsReader so runAnalysis can
// fan out per-control ExecutionRequests for catalog-backed jobs. A nil (or
// unset) reader leaves Controls/CatalogID empty, matching document-backed
// job behavior.
func WithCatalogControlsReader(r CatalogControlsReader) Option {
	return func(s *Service) {
		s.catalogControls = r
	}
}

// WithEmbeddingsReader wires an EmbeddingsReader so runSynthesis can read
// per-model similarity matrices for catalog-backed jobs. A nil (or unset)
// reader leaves the similarity-matrix step a no-op, matching document-backed
// job behavior (synthesis proceeds with SimilarityCount=0 per pair).
func WithEmbeddingsReader(r EmbeddingsReader) Option {
	return func(s *Service) {
		s.embeddingsReader = r
	}
}

// WithEmbeddingConfig sets the embedding configuration runSynthesis uses to
// determine which models' similarity matrices to read.
func WithEmbeddingConfig(cfg config.EmbeddingConfig) Option {
	return func(s *Service) {
		s.embeddingConfig = cfg
	}
}
