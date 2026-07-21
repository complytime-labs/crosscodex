package pipeline

import (
	"context"
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	pipelineattestation "github.com/complytime-labs/crosscodex/internal/pipeline/attestation"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

// Service implements pb.PipelineServiceServer and gateway.PipelineBackend.
type Service struct {
	pb.UnimplementedPipelineServiceServer

	store        Store
	engine       *analysis.Engine
	registry     *analyzer.Registry
	synthesis    SynthesisExecutor
	attestor     attestation.Generator
	attConverter *pipelineattestation.Converter
	bus          natsbus.Client
	storage      storage.Provider
	cfg          config.PipelineConfig
	attCfg       config.AttestationConfig
	logger       *slog.Logger
	tracer       trace.Tracer

	tracerProvider trace.TracerProvider
	meterProvider  metric.MeterProvider

	jobCounter  metric.Int64Counter
	jobDuration metric.Float64Histogram

	mu      sync.Mutex
	wg      sync.WaitGroup
	running map[string]context.CancelFunc
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
	opts ...Option,
) *Service {
	s := &Service{
		store:        store,
		engine:       engine,
		registry:     registry,
		synthesis:    synth,
		attestor:     attestor,
		attConverter: converter,
		bus:          bus,
		storage:      storageProvider,
		cfg:          cfg,
		attCfg:       attCfg,
		logger:       slog.Default(),
		running:      make(map[string]context.CancelFunc),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

var _ pb.PipelineServiceServer = (*Service)(nil)
var _ gateway.PipelineBackend = (*Service)(nil)
