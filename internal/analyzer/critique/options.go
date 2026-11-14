package critique

import (
	"slices"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Option configures the CritiqueAnalyzer.
type Option func(*CritiqueAnalyzer)

// WithTelemetry enables tracing and metrics for the analyzer. Either
// provider may be nil, and a nil provider is ignored.
func WithTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) Option {
	return func(a *CritiqueAnalyzer) {
		if tp != nil {
			a.tracer = tp.Tracer("crosscodex/internal/analyzer/critique")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			a.verdictCounter, _ = meter.Int64Counter(
				"crosscodex.critique.verdicts",
				metric.WithDescription("Critiqued verdicts by outcome: upheld, split, vetoed or failed"),
			)
			a.duration, _ = meter.Float64Histogram(
				"crosscodex.critique.duration_ms",
				metric.WithDescription("Time to build the critique tasks or aggregate their votes, by phase"),
				metric.WithUnit("ms"),
			)
		}
	}
}

// WithDependencies sets what DependsOn returns: the enabled analyzers among
// relationship and requires, whose verdicts the critique reviews. It has no
// config field of its own. The production registry derives it from
// analysis.relationship.enabled and analysis.requires.enabled. The engine
// skips an analyzer whose dependency is not registered, so a disabled
// analyzer must not be listed. Without this option DependsOn is empty.
func WithDependencies(names ...string) Option {
	return func(a *CritiqueAnalyzer) {
		a.dependsOn = slices.Clone(names)
	}
}
