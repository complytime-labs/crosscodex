package requires

import (
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Option configures the RequiresAnalyzer.
type Option func(*RequiresAnalyzer)

// WithTelemetry enables OpenTelemetry tracing and metrics for the analyzer.
// Either provider may be nil; nil providers are silently ignored.
func WithTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) Option {
	return func(a *RequiresAnalyzer) {
		if tp != nil {
			a.tracer = tp.Tracer("crosscodex/internal/analyzer/requires")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			a.voteCounter, _ = meter.Int64Counter(
				"requires.votes.total",
				metric.WithDescription("Total requires votes by parse status"),
			)
			a.consensusLatency, _ = meter.Float64Histogram(
				"requires.consensus.duration_ms",
				metric.WithDescription("Time to compute consensus per pair"),
			)
			a.pairCounter, _ = meter.Int64Counter(
				"requires.pairs.total",
				metric.WithDescription("Total requires pairs processed"),
			)
		}
	}
}
