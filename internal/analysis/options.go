package analysis

import (
	"log/slog"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Option configures an Engine.
type Option func(*Engine)

// WithTelemetry enables OTel tracing and metrics for the Engine.
// Wiring contract: tp and mp are obtained from telemetry.Init() at the
// service layer, configured via ObservabilityConfig. When using NewWithNATS,
// the providers are also forwarded to the NATSDispatcher and NATSCollector.
func WithTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) Option {
	return func(e *Engine) {
		e.tracerProvider = tp
		e.meterProvider = mp
		if tp != nil {
			e.tracer = tp.Tracer("crosscodex/internal/analysis")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			e.execCounter, _ = meter.Int64Counter(
				"analysis.executions.total",
				metric.WithDescription("Total Execute calls"),
			)
			e.execDuration, _ = meter.Float64Histogram(
				"analysis.execution.duration_ms",
				metric.WithDescription("Engine Execute call duration"),
			)
		}
	}
}

// WithLogger sets the Engine's structured logger.
// Wiring contract: logger is constructed from LoggingConfig at the service
// layer via slog.New(). Defaults to slog.Default() if not provided.
func WithLogger(logger *slog.Logger) Option {
	return func(e *Engine) {
		if logger != nil {
			e.logger = logger
		}
	}
}

// WithStageReporter sets the pipeline stage reporter.
// Wiring contract: typically NewNATSStageReporter(natsClient) constructed
// at the service layer. Defaults to noopReporter if not provided.
func WithStageReporter(r StageReporter) Option {
	return func(e *Engine) {
		if r != nil {
			e.reporter = r
		}
	}
}
