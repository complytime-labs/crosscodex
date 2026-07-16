package synthesis

import (
	"log/slog"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Option configures a Service.
type Option func(*options)

// options holds optional configuration for the Service.
type options struct {
	tp               trace.TracerProvider
	tracer           trace.Tracer
	executionCount   metric.Int64Counter
	errorCount       metric.Int64Counter
	durationHist     metric.Float64Histogram
	pairsRanked      metric.Int64Counter
	viabilityUpdates metric.Int64Counter
	logger           *slog.Logger
}

// WithTelemetry enables OTel tracing and metrics for the Service.
// tp configures span emission; mp is the MeterProvider used to create
// instruments directly using the "crosscodex" meter name, consistent
// with other internal packages.
func WithTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) Option {
	return func(o *options) {
		if tp != nil {
			o.tp = tp
			o.tracer = tp.Tracer("crosscodex/internal/synthesis")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			var err error
			o.executionCount, err = meter.Int64Counter(
				"synthesis.executions.total",
				metric.WithDescription("Total synthesis executions"),
			)
			if err != nil {
				slog.Warn("failed to create execution counter", "error", err)
			}
			o.errorCount, err = meter.Int64Counter(
				"synthesis.errors.total",
				metric.WithDescription("Total synthesis errors by category"),
			)
			if err != nil {
				slog.Warn("failed to create error counter", "error", err)
			}
			o.durationHist, err = meter.Float64Histogram(
				"synthesis.duration_ms",
				metric.WithDescription("Synthesis execution duration"),
			)
			if err != nil {
				slog.Warn("failed to create duration histogram", "error", err)
			}
			o.pairsRanked, err = meter.Int64Counter(
				"synthesis.pairs.ranked.total",
				metric.WithDescription("Total pairs ranked"),
			)
			if err != nil {
				slog.Warn("failed to create pairs ranked counter", "error", err)
			}
			o.viabilityUpdates, err = meter.Int64Counter(
				"synthesis.viability.updates.total",
				metric.WithDescription("Total viability database updates"),
			)
			if err != nil {
				slog.Warn("failed to create viability updates counter", "error", err)
			}
		}
	}
}

// WithLogger sets the Service's structured logger.
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) {
		if logger != nil {
			o.logger = logger
		}
	}
}
