package pipeline

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	pbconnect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	pipelineattestation "github.com/complytime-labs/crosscodex/internal/pipeline/attestation"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

// candidateGenerator is the narrow behavior runAnalysis needs from the
// candidate-generation component (Task 14's *CandidateGenerator). Defining it
// as an interface lets the executor be tested with a fake and lets a nil
// generator cleanly disable the candidate-generation step.
type candidateGenerator interface {
	Generate(ctx context.Context, tenantID, jobID string, jobConfig []byte) error
}

// Service implements pbconnect.PipelineServiceHandler and gateway.PipelineBackend.
type Service struct {
	store            Store
	engine           *analysis.Engine
	registry         *analyzer.Registry
	synthesis        SynthesisExecutor
	candidateGen     candidateGenerator
	catalogControls  CatalogControlsReader
	catalogNames     CatalogNameReader
	embeddingsReader EmbeddingsReader
	embeddingConfig  config.EmbeddingConfig
	attestors        Attestors
	attConverter     *pipelineattestation.Converter
	bus              natsbus.Client
	storage          storage.Provider
	cfg              config.PipelineConfig
	logger           *slog.Logger
	tracer           trace.Tracer

	tracerProvider trace.TracerProvider
	meterProvider  metric.MeterProvider

	jobCounter  metric.Int64Counter
	jobDuration metric.Float64Histogram

	// Result reuse (#243), wired by WithResultCache: the store (read only
	// to verify sealed records), the tenant-overridable
	// analysis.result_cache settings, the llm settings that resolve an
	// empty alias and name each alias's revision, and the client the
	// per-job model probe calls.
	cacheWired bool
	cacheStore resultcache.Store
	cacheCfg   config.ResultCacheConfig
	llmCfg     config.LLMConfig
	probe      llmclient.Client

	// Panel diversity (#260), wired by WithPanelProbe: the client that probes
	// each panel alias, and the counter of checks by outcome.
	panelProbe  PanelProbeClient
	panelChecks metric.Int64Counter

	// Model probe telemetry (#243): probes by kind and outcome, and their
	// duration.
	probeOps      metric.Int64Counter
	probeDuration metric.Float64Histogram

	// Result-cache checkpoint telemetry (#243): operations by outcome,
	// their duration, and how many are in flight.
	checkpointOps       metric.Int64Counter
	checkpointDuration  metric.Float64Histogram
	checkpointGauge     metric.Int64Gauge
	checkpointsInFlight atomic.Int64

	mu      sync.Mutex
	wg      sync.WaitGroup
	running map[string]*runningJob
	// retrying holds the job IDs a RetryJob call is acting on, so
	// concurrent retries of one job are serialised: exactly one proceeds.
	retrying map[string]bool
	stopOnce sync.Once
	stopped  chan struct{}
}

// runningJob is a job this service is executing, guarded by Service.mu.
// completing is set, once, when executeJob claims the job's completed
// transition; from then on CancelJob refuses, so a job ends either cancelled
// or completed, never reported cancelled and then completed. cancelled is
// set by CancelJob: the run is still recording the cancellation, and its
// entry stays until executeJob returns, so no retry can start a second run
// of the job while the first still writes.
type runningJob struct {
	cancel     context.CancelCauseFunc
	completing bool
	cancelled  bool
}

func New(
	store Store,
	engine *analysis.Engine,
	registry *analyzer.Registry,
	synth SynthesisExecutor,
	attestors Attestors,
	converter *pipelineattestation.Converter,
	bus natsbus.Client,
	storageProvider storage.Provider,
	cfg config.PipelineConfig,
	candidateGen candidateGenerator,
	opts ...Option,
) *Service {
	s := &Service{
		store:        store,
		engine:       engine,
		registry:     registry,
		synthesis:    synth,
		candidateGen: candidateGen,
		attestors:    attestors,
		attConverter: converter,
		bus:          bus,
		storage:      storageProvider,
		cfg:          cfg,
		logger:       slog.Default(),
		running:      make(map[string]*runningJob),
		retrying:     make(map[string]bool),
		stopped:      make(chan struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

var _ pbconnect.PipelineServiceHandler = (*Service)(nil)
var _ gateway.PipelineBackend = (*Service)(nil)
