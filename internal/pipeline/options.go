package pipeline

import (
	"log/slog"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
)

// Option configures a Service.
type Option func(*Service)

// WithTelemetry enables OTel tracing and metrics for the pipeline Service.
func WithTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) Option {
	return func(s *Service) {
		s.tracerProvider = tp
		s.meterProvider = mp
		if tp != nil {
			s.tracer = tp.Tracer("crosscodex/internal/pipeline")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			s.jobCounter, _ = meter.Int64Counter("pipeline.jobs.total",
				metric.WithDescription("Total pipeline jobs by status"))
			s.jobDuration, _ = meter.Float64Histogram("pipeline.job.duration_ms",
				metric.WithDescription("Pipeline job duration"))
			s.probeOps, _ = meter.Int64Counter("pipeline.result_cache.probes.total",
				metric.WithDescription("Per-job model probes by kind and outcome (observed: the alias is cached; empty: no usable model name; "+
					"uncached: no usable deployment ID, or only the alias named; failed)"))
			s.probeDuration, _ = meter.Float64Histogram("pipeline.result_cache.probe.duration_ms",
				metric.WithDescription("Per-job model probe duration"), metric.WithUnit("ms"))
			s.panelChecks, _ = meter.Int64Counter("crosscodex.pipeline.panel_checks",
				metric.WithDescription("Panel diversity checks by stage and outcome (independent; shared: two aliases answered as one model; "+
					"allowed: shared or unknown but analysis.<stage>.allow_shared_panel_models is set; unknown: an alias's upstream model is not named; "+
					"failed: a probe call failed)"))
			s.checkpointOps, _ = meter.Int64Counter("pipeline.result_cache.checkpoints.total",
				metric.WithDescription("Result-cache checkpoint operations (sign, store, verify) by outcome"))
			s.checkpointDuration, _ = meter.Float64Histogram("pipeline.result_cache.checkpoint.duration_ms",
				metric.WithDescription("Result-cache checkpoint operation duration"), metric.WithUnit("ms"))
			s.checkpointGauge, _ = meter.Int64Gauge("pipeline.result_cache.checkpoints.in_flight",
				metric.WithDescription("Result-cache checkpoint operations in progress"))
		}
	}
}

// WithResultCache wires LLM result reuse (#243). store is the result cache
// store, the same one the engine is built with (analysis.WithResultCache);
// the pipeline reads it only to verify a stage's sealed record against its
// checkpoint, when attesting a stage an earlier run completed and for
// GetJob's cache summaries. cfg is
// analysis.result_cache (enabled and lookup_batch_size, per tenant); llm is
// the llm config, whose default_model and embedding_model resolve an empty
// alias exactly as the worker does and whose model_revisions join every
// cache key; probe is the client the per-job model probe calls, which
// crosscodexd wires to its shared, audited LLM client. The engine must be
// built with analysis.WithResultCache over the result cache store. Without
// this option jobs neither read nor write the cache.
func WithResultCache(store resultcache.Store, cfg config.ResultCacheConfig, llm config.LLMConfig, probe llmclient.Client) Option {
	return func(s *Service) {
		s.cacheWired = true
		s.cacheStore = store
		s.cacheCfg = cfg
		s.llmCfg = llm
		s.probe = probe
	}
}

// WithPanelProbe wires the client that probes the aliases of each panel
// analyzer (analyzer.Panelled) before the analysis runs, so the job is refused
// when two aliases answer as the same upstream model or an alias's upstream
// model is unknown (#260). The client must also read the gateway's model
// catalog (PanelProbeClient), because a LiteLLM gateway names the model
// behind an alias only there; a client that cannot is refused by the compiler,
// not at call time. Production wires it in the daemon
// (cmd/crosscodexd llmProbeOptions) to its probe client; the embedded start
// builds no pipeline and does not. Without it the check is skipped and each
// panel stage's record says "panel independence unchecked".
func WithPanelProbe(c PanelProbeClient) Option {
	return func(s *Service) {
		s.panelProbe = c
	}
}

// WithLogger sets the Service's structured logger.
func WithLogger(logger *slog.Logger) Option {
	return func(s *Service) {
		if logger != nil {
			s.logger = logger
		}
	}
}

// WithCatalogControlsReader wires a CatalogControlsReader so runAnalysis can
// fan out per-control ExecutionRequests over the job's catalog. A nil (or
// unset) reader leaves Controls empty, so per-control analyzers produce zero
// tasks, the same as a catalog with zero controls.
func WithCatalogControlsReader(r CatalogControlsReader) Option {
	return func(s *Service) {
		s.catalogControls = r
	}
}

// WithCatalogNameReader wires a CatalogNameReader. CreateJob (and RetryJob's
// new job) use it to confirm a job's catalogs are its tenant's and to record
// their names and versions in the job's config; runAnalysis uses it to
// confirm the catalogs still exist and, for a config stored before those
// snapshots, to give the pair analyzers each control's catalog name as its
// framework. Without one (nil or unset), CreateJob and RetryJob's new job
// refuse every job (fail closed); runAnalysis, resuming a job already
// stored, leaves every framework parameter empty.
func WithCatalogNameReader(r CatalogNameReader) Option {
	return func(s *Service) {
		s.catalogNames = r
	}
}

// WithEmbeddingsReader wires an EmbeddingsReader so runSynthesis can read
// per-model similarity matrices for the job's catalog. A nil (or unset)
// reader leaves the similarity-matrix step a no-op, and synthesis proceeds
// with SimilarityCount=0 per pair.
func WithEmbeddingsReader(r EmbeddingsReader) Option {
	return func(s *Service) {
		s.embeddingsReader = r
	}
}

// WithEmbeddingConfig sets the embedding configuration runSynthesis uses to
// determine which models' similarity matrices to read.
func WithEmbeddingConfig(cfg config.EmbeddingConfig) Option {
	return func(s *Service) {
		s.embeddingConfig = cfg
	}
}

// PanelProbeWired reports whether WithPanelProbe was applied.
func (s *Service) PanelProbeWired() bool { return s.panelProbe != nil }
