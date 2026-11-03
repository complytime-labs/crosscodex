package worker

import (
	"log/slog"
	"time"

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
			w.droppedCounter, err = meter.Int64Counter(
				"worker.tasks.dropped",
				metric.WithDescription("Tasks dropped before any LLM call, by reason and task_type"),
			)
			if err != nil {
				slog.Warn("failed to create dropped task counter", "error", err)
			}
			w.abandonedGauge, err = meter.Int64Gauge(
				"worker.abandoned_collections",
				metric.WithDescription("Abandoned collections the worker currently remembers"),
			)
			if err != nil {
				slog.Warn("failed to create abandoned collections gauge", "error", err)
			}
		}
	}
}

// WithAbandonTTL sets how long the worker remembers an abandoned collection
// and drops its tasks. Wiring contract: fed by pipeline.stage_timeout
// (config.PipelineConfig.StageTimeout), the longest a collection can live,
// which validation keeps in (0, 168h]. Start refuses a worker without a
// positive TTL (ErrNoAbandonTTL).
func WithAbandonTTL(d time.Duration) Option {
	return func(w *Worker) {
		w.abandonTTL = d
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
