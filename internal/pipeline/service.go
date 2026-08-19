package pipeline

import (
	"context"
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	pbconnect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	pipelineattestation "github.com/complytime-labs/crosscodex/internal/pipeline/attestation"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
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
	embeddingsReader EmbeddingsReader
	embeddingConfig  config.EmbeddingConfig
	attestor         attestation.Generator
	attConverter     *pipelineattestation.Converter
	bus              natsbus.Client
	storage          storage.Provider
	cfg              config.PipelineConfig
	attCfg           config.AttestationConfig
	logger           *slog.Logger
	tracer           trace.Tracer

	tracerProvider trace.TracerProvider
	meterProvider  metric.MeterProvider

	jobCounter  metric.Int64Counter
	jobDuration metric.Float64Histogram

	mu       sync.Mutex
	wg       sync.WaitGroup
	running  map[string]context.CancelFunc
	stopOnce sync.Once
	stopped  chan struct{}
}

func New(
	store Store,
	engine *analysis.Engine,
	registry *analyzer.Registry,
	synth SynthesisExecutor,
	attestor attestation.Generator,
	converter *pipelineattestation.Converter,
	bus natsbus.Client,
	storageProvider storage.Provider,
	cfg config.PipelineConfig,
	attCfg config.AttestationConfig,
	candidateGen candidateGenerator,
	opts ...Option,
) *Service {
	s := &Service{
		store:        store,
		engine:       engine,
		registry:     registry,
		synthesis:    synth,
		candidateGen: candidateGen,
		attestor:     attestor,
		attConverter: converter,
		bus:          bus,
		storage:      storageProvider,
		cfg:          cfg,
		attCfg:       attCfg,
		logger:       slog.Default(),
		running:      make(map[string]context.CancelFunc),
		stopped:      make(chan struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

var _ pbconnect.PipelineServiceHandler = (*Service)(nil)
var _ gateway.PipelineBackend = (*Service)(nil)
