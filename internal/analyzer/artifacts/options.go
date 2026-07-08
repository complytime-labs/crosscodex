package artifacts

import (
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Option configures the ArtifactsAnalyzer.
type Option func(*ArtifactsAnalyzer)

// WithTelemetry enables OpenTelemetry tracing and metrics for the analyzer.
// Either provider may be nil; nil providers are silently ignored.
func WithTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) Option {
	return func(a *ArtifactsAnalyzer) {
		if tp != nil {
			a.tracer = tp.Tracer("crosscodex/internal/analyzer/artifacts")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			a.extractionCounter, _ = meter.Int64Counter(
				"artifacts.extractions.total",
				metric.WithDescription("Total artifact extractions by status"),
			)
		}
	}
}
