package pipeline

import (
	"fmt"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
	"github.com/complytime-labs/crosscodex/internal/analyzer/classify"
	"github.com/complytime-labs/crosscodex/internal/analyzer/embedding"
	"github.com/complytime-labs/crosscodex/internal/analyzer/relationship"
	"github.com/complytime-labs/crosscodex/internal/analyzer/requires"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
)

// NewProductionRegistry builds and registers all five real analyzers
// (classify, embedding, artifacts, requires, relationship) with their
// production dependencies, per the assembly pattern documented in
// internal/pipeline/doc.go. Consumed directly by the end-to-end test in
// this package and by crosscodexd's pipeline-role bootstrap.
//
// tp and mp are forwarded to the registry, to each analyzer, and to the two
// candidate providers, so every span and metric they emit shares the
// caller's tracer/meter providers. Either may be nil, which preserves the
// previous uninstrumented behaviour.
func NewProductionRegistry(
	llm llmclient.Client,
	vectors vectordb.VectorDB,
	storageProvider storage.Provider,
	prompts prompt.Registry,
	conn db.TenantConnection,
	cfg config.AnalysisConfig,
	tp trace.TracerProvider,
	mp metric.MeterProvider,
	opts ...analyzer.RegistryOption,
) (*analyzer.Registry, error) {
	registryOpts := append([]analyzer.RegistryOption{analyzer.WithTelemetry(tp, mp)}, opts...)
	reg := analyzer.NewRegistry(registryOpts...)

	if err := analyzer.Register[*pb.Control](reg, classify.New(llm, prompts, cfg.Classification, classify.WithTelemetry(tp, mp))); err != nil {
		return nil, fmt.Errorf("NewProductionRegistry: registering classify: %w", err)
	}
	if err := analyzer.Register[*pb.Control](reg, embedding.New(llm, vectors, storageProvider, cfg.Embedding, cfg.Relationship, embedding.WithTelemetry(tp, mp))); err != nil {
		return nil, fmt.Errorf("NewProductionRegistry: registering embedding: %w", err)
	}
	if err := analyzer.Register[*pb.Control](reg, artifacts.New(llm, prompts, cfg.Artifacts, artifacts.WithTelemetry(tp, mp))); err != nil {
		return nil, fmt.Errorf("NewProductionRegistry: registering artifacts: %w", err)
	}

	requiresProvider := NewRequiresCandidateProvider(conn, WithCandidateTelemetry(tp, mp))
	if err := analyzer.Register[*pb.Control](reg, requires.New(llm, prompts, requiresProvider, cfg.Requires, requires.WithTelemetry(tp, mp))); err != nil {
		return nil, fmt.Errorf("NewProductionRegistry: registering requires: %w", err)
	}

	relationshipProvider := NewRelationshipCandidateProvider(conn, WithRelationshipCandidateTelemetry(tp, mp))
	if err := analyzer.Register[*pb.Control](reg, relationship.New(llm, prompts, relationshipProvider, cfg.Relationship, relationship.WithTelemetry(tp, mp))); err != nil {
		return nil, fmt.Errorf("NewProductionRegistry: registering relationship: %w", err)
	}

	return reg, nil
}
