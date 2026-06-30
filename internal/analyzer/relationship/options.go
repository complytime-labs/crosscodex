package relationship

import (
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Option configures the RelationshipAnalyzer.
type Option func(*RelationshipAnalyzer)

// WithTelemetry enables OpenTelemetry tracing and metrics for the analyzer.
// Either provider may be nil; nil providers are silently ignored.
func WithTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) Option {
	return func(a *RelationshipAnalyzer) {
		if tp != nil {
			a.tracer = tp.Tracer("crosscodex/internal/analyzer/relationship")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			a.voteCounter, _ = meter.Int64Counter(
				"relationship.votes.total",
				metric.WithDescription("Total relationship votes by parse status"),
			)
			a.consensusLatency, _ = meter.Float64Histogram(
				"relationship.consensus.duration_ms",
				metric.WithDescription("Time to compute consensus per pair"),
			)
			a.pairCounter, _ = meter.Int64Counter(
				"relationship.pairs.total",
				metric.WithDescription("Total relationship pairs processed"),
			)
		}
	}
}
