package embedding

import (
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Option configures the EmbeddingAnalyzer.
type Option func(*EmbeddingAnalyzer)

// WithTelemetry enables OpenTelemetry tracing and metrics for the analyzer.
func WithTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) Option {
	return func(a *EmbeddingAnalyzer) {
		if tp != nil {
			a.tracer = tp.Tracer("crosscodex/internal/analyzer/embedding")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			a.embedCounter, _ = meter.Int64Counter(
				"embedding.operations.total",
				metric.WithDescription("Total embedding operations by model and status"),
			)
		}
	}
}
