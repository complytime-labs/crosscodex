package pipeline

import (
	"fmt"
	"slices"
	"strings"

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

// analyzerEnabledSettings maps each production analyzer to the config
// setting that enables it, for errors that tell an operator what to change.
var analyzerEnabledSettings = map[string]string{
	"classify":     "analysis.classification.enabled",
	"embedding":    "analysis.embedding.enabled",
	"artifacts":    "analysis.artifacts.enabled",
	"requires":     "analysis.requires.enabled",
	"relationship": "analysis.relationship.enabled",
}

// NewProductionRegistry builds and registers the real analyzers (classify,
// embedding, artifacts, requires, relationship) that cfg enables, with their
// production dependencies, per the assembly pattern documented in
// internal/pipeline/doc.go. A disabled analyzer is not registered, so it gets
// no job stage, never runs and makes no LLM calls. Consumed directly by the
// end-to-end test in this package and by crosscodexd's pipeline-role
// bootstrap.
//
// An enabled analyzer whose dependency is disabled (embedding needs classify;
// requires and relationship need embedding) is refused with
// analyzer.ErrMissingDependency, naming both settings. Config validation
// rejects the same combinations; this keeps a caller that skips it from
// starting with an analyzer graph no job can run.
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

	analyzers := []struct {
		name     string
		enabled  bool
		register func() error
	}{
		{"classify", cfg.Classification.Enabled, func() error {
			return analyzer.Register[*pb.Control](reg, classify.New(llm, prompts, cfg.Classification, classify.WithTelemetry(tp, mp)))
		}},
		{"embedding", cfg.Embedding.Enabled, func() error {
			return analyzer.Register[*pb.Control](reg, embedding.New(llm, vectors, storageProvider, cfg.Embedding, embedding.WithTelemetry(tp, mp)))
		}},
		{"artifacts", cfg.Artifacts.Enabled, func() error {
			return analyzer.Register[*pb.Control](reg, artifacts.New(llm, prompts, cfg.Artifacts, artifacts.WithTelemetry(tp, mp)))
		}},
		{"requires", cfg.Requires.Enabled, func() error {
			provider := NewRequiresCandidateProvider(conn, WithCandidateTelemetry(tp, mp))
			return analyzer.Register[*pb.Control](reg, requires.New(llm, prompts, provider, cfg.Requires, requires.WithTelemetry(tp, mp)))
		}},
		{"relationship", cfg.Relationship.Enabled, func() error {
			provider := NewRelationshipCandidateProvider(conn, WithRelationshipCandidateTelemetry(tp, mp))
			return analyzer.Register[*pb.Control](reg, relationship.New(llm, prompts, provider, cfg.Relationship, relationship.WithTelemetry(tp, mp)))
		}},
	}
	for _, a := range analyzers {
		if !a.enabled {
			continue
		}
		if err := a.register(); err != nil {
			return nil, fmt.Errorf("NewProductionRegistry: registering %s: %w", a.name, err)
		}
	}

	// Sorted, so the error names the same analyzer every run.
	registered := reg.All()
	slices.SortFunc(registered, func(a, b analyzer.RegisteredAnalyzer) int { return strings.Compare(a.Name(), b.Name()) })
	for _, a := range registered {
		for _, dep := range a.DependsOn() {
			if _, err := reg.Get(dep); err != nil {
				return nil, fmt.Errorf("NewProductionRegistry: analyzer %q depends on %q, which is disabled: %w; "+
					"set %s to true, or set %s to false",
					a.Name(), dep, analyzer.ErrMissingDependency, analyzerEnabledSettings[dep], analyzerEnabledSettings[a.Name()])
			}
		}
	}

	return reg, nil
}
