package analysis

import (
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/resultcache"
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
			e.cacheLookups, _ = meter.Int64Counter("analysis.result_cache.lookups.total",
				metric.WithDescription("Result cache keys looked up"))
			e.cacheHits, _ = meter.Int64Counter("analysis.result_cache.hits.total",
				metric.WithDescription("Tasks served from the result cache instead of a worker"))
			e.cachePuts, _ = meter.Int64Counter("analysis.result_cache.puts.total",
				metric.WithDescription("Results stored in the result cache"))
			e.cachePutErrors, _ = meter.Int64Counter("analysis.result_cache.put_errors.total",
				metric.WithDescription("Results that could not be stored in the result cache"))
			e.cachePutsRefused, _ = meter.Int64Counter("analysis.result_cache.puts_refused.total",
				metric.WithDescription("Successful results not stored in the result cache (attribute reason: "+
					"unusable_result, the analyzer would discard it; model_mismatch, another model than the probe observed answered; "+
					"deployment_mismatch, another deployment than the probe reached answered)"))
			e.cacheIntegrityFailures, _ = meter.Int64Counter("analysis.result_cache.integrity_failures.total",
				metric.WithDescription("Cached results, checkpoints or stage records that failed an integrity check, "+
					"and checkpoints signed by a key no longer trusted (attribute reason)"))
			e.cacheLookupDuration, _ = meter.Float64Histogram("analysis.result_cache.lookup.duration_ms",
				metric.WithDescription("Duration of one stage's result cache lookups"), metric.WithUnit("ms"))
			e.cacheInFlight, _ = meter.Int64Gauge("analysis.result_cache.entries_in_flight",
				metric.WithDescription("Result cache keys being looked up or stored"))
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

// WithStageTimeout sets the hard cap on each analyzer stage's collection.
// Wiring contract: fed by pipeline.stage_timeout (config.PipelineConfig),
// which validation keeps in (0, 168h]. Without it every collection the
// NATSCollector prepares fails with ErrNoStageTimeout, so a stage can never
// run unbounded.
func WithStageTimeout(d time.Duration) Option {
	return func(e *Engine) {
		e.stageTimeout = d
	}
}

// WithResultCache wires the LLM result cache store (#243). Wiring
// contract: crosscodexd passes pkg/resultcache/pgstore over the app_user
// tenant pool when it builds the pipeline; there is no config field for
// the store itself. Whether an execution reads or writes it is per request
// (ExecutionRequest.Cache), which the pipeline resolves from
// analysis.result_cache and the job's no_cache. A request in ModeEnabled or
// ModeBypassed on an engine without a store fails Execute
// (ErrResultCacheNotWired).
func WithResultCache(store resultcache.Store) Option {
	return func(e *Engine) {
		e.cacheStore = store
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
