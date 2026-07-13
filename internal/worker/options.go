package worker

import (
	"log/slog"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Option configures a Worker.
type Option func(*Worker)

// WithTelemetry enables OTel tracing and metrics for the Worker.
func WithTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) Option {
	return func(w *Worker) {
		if tp != nil {
			w.tracer = tp.Tracer("crosscodex/internal/worker")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			var err error
			w.taskCounter, err = meter.Int64Counter(
				"worker.tasks.processed",
				metric.WithDescription("Tasks processed by the worker"),
			)
			if err != nil {
				slog.Warn("failed to create task counter", "error", err)
			}
			w.taskDuration, err = meter.Float64Histogram(
				"worker.task.duration_ms",
				metric.WithDescription("End-to-end task processing duration"),
			)
			if err != nil {
				slog.Warn("failed to create task duration histogram", "error", err)
			}
			w.errorCounter, err = meter.Int64Counter(
				"worker.errors",
				metric.WithDescription("Worker errors by category"),
			)
			if err != nil {
				slog.Warn("failed to create error counter", "error", err)
			}
			w.llmLatency, err = meter.Float64Histogram(
				"worker.llm.latency_ms",
				metric.WithDescription("LLM API call latency"),
			)
			if err != nil {
				slog.Warn("failed to create LLM latency histogram", "error", err)
			}
		}
	}
}

// WithLogger sets the Worker's structured logger.
func WithLogger(logger *slog.Logger) Option {
	return func(w *Worker) {
		if logger != nil {
			w.logger = logger
		}
	}
}
