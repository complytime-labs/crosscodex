package pipeline

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/llmclient/llmclienttest"
	"github.com/complytime-labs/crosscodex/pkg/prompt/prompttest"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/vectordb/vectordbtest"
)

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
	// Every transaction enters the tenant and queries zero rows, so the
	// requires/relationship candidate providers' Candidates() reaches a
	// successful empty result without a real database, exercising their
	// query span.
	conn := &dbtest.Conn{Tx: &dbtest.Tx{
		ExecFunc:  func(context.Context, string, ...any) error { return nil },
		QueryFunc: func(context.Context, string, ...any) (db.Rows, error) { return dbtest.Rows(nil), nil },
	}}
	return NewProductionRegistry(&llmclienttest.Fake{}, &vectordbtest.Fake{}, &storagetest.Fake{}, &prompttest.Fake{}, conn, validAnalysisConfig(), tp, mp)
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
				// result, which is exactly what the dbtest.Tx above produces.
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
