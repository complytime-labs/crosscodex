package pipeline

import (
	"context"
	"fmt"
	"io"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
)

// fakeLLMClient is a minimal no-op llmclient.Client for constructor wiring
// tests that never invoke Complete/Embed.
type fakeLLMClient struct{}

func (f *fakeLLMClient) Complete(ctx context.Context, req *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
	return nil, nil
}

func (f *fakeLLMClient) Embed(ctx context.Context, req *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
	return nil, nil
}

func (f *fakeLLMClient) Health(ctx context.Context) error { return nil }

func (f *fakeLLMClient) Close() error { return nil }

// fakeVectorDB is a minimal no-op vectordb.VectorDB for constructor wiring
// tests that never invoke embedding storage or search.
type fakeVectorDB struct{}

func (f *fakeVectorDB) StoreEmbedding(ctx context.Context, tenant string, embedding vectordb.Embedding) error {
	return nil
}

func (f *fakeVectorDB) StoreBatch(ctx context.Context, tenant string, embeddings []vectordb.Embedding) error {
	return nil
}

func (f *fakeVectorDB) FindSimilar(ctx context.Context, tenant string, query vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
	return nil, nil
}

func (f *fakeVectorDB) DeleteByModel(ctx context.Context, tenant, catalogID, model string) error {
	return nil
}

// fakeStorage is a minimal no-op storage.Provider for constructor wiring
// tests that never read or write objects.
type fakeStorage struct{}

func (f *fakeStorage) Get(ctx context.Context, key string) (io.ReadCloser, error) { return nil, nil }

func (f *fakeStorage) Put(ctx context.Context, key string, data io.Reader) error { return nil }

func (f *fakeStorage) Delete(ctx context.Context, key string) error { return nil }

func (f *fakeStorage) List(ctx context.Context, prefix string) ([]storage.ObjectMetadata, error) {
	return nil, nil
}

func (f *fakeStorage) Exists(ctx context.Context, key string) (bool, error) { return false, nil }

func (f *fakeStorage) Stat(ctx context.Context, key string) (*storage.ObjectMetadata, error) {
	return nil, nil
}

func (f *fakeStorage) Close() error { return nil }

// fakePromptRegistry is a minimal no-op prompt.Registry for constructor
// wiring tests that never resolve or render prompts.
type fakePromptRegistry struct{}

func (f *fakePromptRegistry) Resolve(ctx context.Context, name string) (*prompt.PromptSpec, error) {
	return nil, nil
}

func (f *fakePromptRegistry) Render(ctx context.Context, name string, vars map[string]string) (*prompt.ResolvedPrompt, error) {
	return nil, nil
}

func (f *fakePromptRegistry) List(ctx context.Context) ([]string, error) { return nil, nil }

func (f *fakePromptRegistry) Layers(ctx context.Context, name string) ([]prompt.LayerInfo, error) {
	return nil, nil
}

// fakeTenantConn is a minimal db.TenantConnection for constructor wiring
// tests. Begin returns a fakeCandidateTx so that the requires/relationship
// candidate providers' Candidates() query can run to completion (zero rows)
// without a real database, exercising their query span.
type fakeTenantConn struct{}

func (f *fakeTenantConn) Begin(ctx context.Context) (db.Transaction, error) {
	return &fakeCandidateTx{}, nil
}

func (f *fakeTenantConn) Query(ctx context.Context, query string, args ...any) (db.Rows, error) {
	return nil, nil
}

func (f *fakeTenantConn) QueryRow(ctx context.Context, query string, args ...any) db.Row {
	return nil
}

func (f *fakeTenantConn) Exec(ctx context.Context, query string, args ...any) error { return nil }

func (f *fakeTenantConn) Close() error { return nil }

// fakeCandidateTx is a minimal db.Transaction returning zero rows from
// Query, letting RequiresCandidateProvider/RelationshipCandidateProvider's
// Candidates() reach a successful empty result instead of panicking on a
// nil transaction.
type fakeCandidateTx struct{}

func (f *fakeCandidateTx) Commit() error   { return nil }
func (f *fakeCandidateTx) Rollback() error { return nil }

func (f *fakeCandidateTx) Query(ctx context.Context, query string, args ...any) (db.Rows, error) {
	return &fakeEmptyRows{}, nil
}

func (f *fakeCandidateTx) QueryRow(ctx context.Context, query string, args ...any) db.Row {
	return nil
}

func (f *fakeCandidateTx) Exec(ctx context.Context, query string, args ...any) error { return nil }

// fakeEmptyRows is a db.Rows with no rows.
type fakeEmptyRows struct{}

func (f *fakeEmptyRows) Next() bool             { return false }
func (f *fakeEmptyRows) Scan(dest ...any) error { return fmt.Errorf("fakeEmptyRows: no current row") }
func (f *fakeEmptyRows) Close() error           { return nil }
func (f *fakeEmptyRows) Err() error             { return nil }

// validAnalysisConfig returns a minimal config.AnalysisConfig that satisfies
// every analyzer's construction-time validation.
func validAnalysisConfig() config.AnalysisConfig {
	return config.AnalysisConfig{
		Classification: config.ClassificationConfig{Model: "m", MaxTextLength: 100},
		Embedding:      config.EmbeddingConfig{Models: []string{"m"}, MaxChars: 100, BatchSize: 1},
		Relationship:   config.RelationshipConfig{Models: []string{"m"}, SamplesPerModel: 1, MaxSourceChars: 100, MaxTargetChars: 100, MaxTokens: 100},
		Requires:       config.RequiresConfig{Models: []string{"m"}, SamplesPerModel: 1, ConsensusThreshold: 0.5, MaxSourceChars: 100, MaxTargetChars: 100, MaxTokens: 100},
		Artifacts:      config.ArtifactsConfig{Models: []string{"m"}, SamplesPerModel: 1, MaxTokens: 100, MaxTextChars: 100, FuzzyThreshold: 0.6},
	}
}

func newProductionRegistryForTest(tp trace.TracerProvider, mp metric.MeterProvider) (*analyzer.Registry, error) {
	return NewProductionRegistry(&fakeLLMClient{}, &fakeVectorDB{}, &fakeStorage{}, &fakePromptRegistry{}, &fakeTenantConn{}, validAnalysisConfig(), tp, mp)
}

var _ = Describe("NewProductionRegistry", func() {
	It("registers all five analyzers and builds a DAG with nil telemetry providers", func() {
		reg, err := newProductionRegistryForTest(nil, nil)
		Expect(err).NotTo(HaveOccurred())

		for _, name := range []string{"classify", "embedding", "artifacts", "requires", "relationship"} {
			_, err := reg.Get(name)
			Expect(err).NotTo(HaveOccurred(), "registry missing analyzer %q", name)
		}

		_, err = reg.BuildDAG(context.Background())
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("telemetry forwarding", func() {
		It("records a registration on the registry's own counter for each analyzer", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(tp.Shutdown, context.Background())

			_, err = newProductionRegistryForTest(tp.TracerProvider(), tp.MeterProvider())
			Expect(err).NotTo(HaveOccurred())

			rm := tp.GetMetrics()
			counterMetric := telemetrytest.FindMetric(rm, "analyzer.registrations.total")
			Expect(counterMetric).NotTo(BeNil(), "expected analyzer.registrations.total metric")
			count, err := telemetrytest.CounterValue(counterMetric)
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(5)))
		})

		DescribeTable("forwards the tracer provider into each registered analyzer",
			func(name string) {
				tp, err := telemetrytest.NewTestProvider()
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(tp.Shutdown, context.Background())

				reg, err := newProductionRegistryForTest(tp.TracerProvider(), tp.MeterProvider())
				Expect(err).NotTo(HaveOccurred())

				a, err := reg.Get(name)
				Expect(err).NotTo(HaveOccurred())

				// GenerateWork fails fast on the missing tenant in ctx; the
				// span is started before that check runs, so the span is
				// still recorded. Assert the specific failure explicitly so
				// a different, unrelated early failure can't masquerade as
				// this one.
				_, err = a.GenerateWorkFromProto(context.Background(), &pb.Control{}, analyzer.AnalyzerConfig{})
				Expect(err).To(MatchError(ContainSubstring("no tenant in context")))

				spans := tp.GetSpans()
				span := telemetrytest.FindSpan(spans, name+".GenerateWork")
				Expect(span).NotTo(BeNil(), "expected a %s.GenerateWork span", name)
				Expect(span.InstrumentationScope().Name).To(Equal("crosscodex/internal/analyzer/" + name))
			},
			Entry("classify", "classify"),
			Entry("embedding", "embedding"),
			Entry("artifacts", "artifacts"),
			Entry("requires", "requires"),
			Entry("relationship", "relationship"),
		)

		DescribeTable("forwards the tracer provider into the candidate provider behind requires/relationship",
			func(analyzerName, querySpanName string) {
				tp, err := telemetrytest.NewTestProvider()
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(tp.Shutdown, context.Background())

				reg, err := newProductionRegistryForTest(tp.TracerProvider(), tp.MeterProvider())
				Expect(err).NotTo(HaveOccurred())

				a, err := reg.Get(analyzerName)
				Expect(err).NotTo(HaveOccurred())

				ctx := testspecs.SetupTenantContext("acme-corp")
				cfg := analyzer.AnalyzerConfig{Parameters: map[string]string{"job_id": "job-1"}}

				// Zero candidate pairs is itself a successful outcome (see
				// requires.GenerateWork / relationship.GenerateWork): both
				// return (nil, nil) once Candidates() succeeds with an empty
				// result, which is exactly what fakeCandidateTx produces.
				_, err = a.GenerateWorkFromProto(ctx, &pb.Control{}, cfg)
				Expect(err).NotTo(HaveOccurred())

				spans := tp.GetSpans()
				span := telemetrytest.FindSpan(spans, querySpanName)
				Expect(span).NotTo(BeNil(), "expected a %s span", querySpanName)
				Expect(span.InstrumentationScope().Name).To(Equal("crosscodex/internal/pipeline"))
			},
			Entry("requires", "requires", "requires.candidates.query"),
			Entry("relationship", "relationship", "relationship.candidates.query"),
		)
	})
})
