package classify

import (
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Option configures the ClassifyAnalyzer.
type Option func(*ClassifyAnalyzer)

// WithTelemetry enables OpenTelemetry tracing and metrics for the analyzer.
// The tracer is used for spans on GenerateWork and Aggregate. The meter
// provides a counter for classification operations and a histogram for
// prompt text length. Either provider may be nil; nil providers are silently
// ignored.
func WithTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) Option {
	return func(a *ClassifyAnalyzer) {
		if tp != nil {
			a.tracer = tp.Tracer("crosscodex/internal/analyzer/classify")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			a.classifyCounter, _ = meter.Int64Counter(
				"classify.operations.total",
				metric.WithDescription("Total classification operations by result"),
			)
			a.textLenHistogram, _ = meter.Float64Histogram(
				"classify.prompt.text_length",
				metric.WithDescription("Rune count of sanitized prompt text"),
			)
		}
	}
}
