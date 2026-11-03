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

// validAnalysisConfig returns a config.AnalysisConfig with all five analyzers
// enabled that satisfies every analyzer's construction-time validation.
func validAnalysisConfig() config.AnalysisConfig {
	return config.AnalysisConfig{
		Classification: config.ClassificationConfig{Enabled: true, Model: "m", MaxTextLength: 100},
		Embedding:      config.EmbeddingConfig{Enabled: true, Models: []string{"m"}, MaxChars: 100, BatchSize: 1},
		Relationship:   config.RelationshipConfig{Enabled: true, Models: []string{"m"}, SamplesPerModel: 1, MaxSourceChars: 100, MaxTargetChars: 100, MaxTokens: 100},
		Requires:       config.RequiresConfig{Enabled: true, Models: []string{"m"}, SamplesPerModel: 1, ConsensusThreshold: 0.5, MaxSourceChars: 100, MaxTargetChars: 100, MaxTokens: 100},
		Artifacts:      config.ArtifactsConfig{Enabled: true, Models: []string{"m"}, SamplesPerModel: 1, MaxTokens: 100, MaxTextChars: 100, FuzzyThreshold: 0.6},
	}
}

func newProductionRegistryForTest(tp trace.TracerProvider, mp metric.MeterProvider) (*analyzer.Registry, error) {
	return newProductionRegistryWithConfig(validAnalysisConfig(), tp, mp)
}

func newProductionRegistryWithConfig(cfg config.AnalysisConfig, tp trace.TracerProvider, mp metric.MeterProvider) (*analyzer.Registry, error) {
	// Every transaction enters the tenant and queries zero rows, so the
	// requires/relationship candidate providers' Candidates() reaches a
	// successful empty result without a real database, exercising their
	// query span.
	conn := &dbtest.Conn{Tx: &dbtest.Tx{
		ExecFunc:  func(context.Context, string, ...any) error { return nil },
		QueryFunc: func(context.Context, string, ...any) (db.Rows, error) { return dbtest.Rows(nil), nil },
	}}
	return NewProductionRegistry(&llmclienttest.Fake{}, &vectordbtest.Fake{}, &storagetest.Fake{}, &prompttest.Fake{}, conn, cfg, tp, mp)
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

	DescribeTable("registers exactly the enabled analyzers, and their DAG builds",
		func(enable func(*config.AnalysisConfig), want []string) {
			cfg := validAnalysisConfig()
			cfg.Classification.Enabled = false
			cfg.Embedding.Enabled = false
			cfg.Artifacts.Enabled = false
			cfg.Requires.Enabled = false
			cfg.Relationship.Enabled = false
			enable(&cfg)

			reg, err := newProductionRegistryWithConfig(cfg, nil, nil)
			Expect(err).NotTo(HaveOccurred())

			var got []string
			for _, a := range reg.All() {
				got = append(got, a.Name())
			}
			Expect(got).To(ConsistOf(want))
			dag, err := reg.BuildDAG(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(dag.Order()).To(ConsistOf(want))
		},
		Entry("none", func(*config.AnalysisConfig) {}, []string{}),
		Entry("classify", func(c *config.AnalysisConfig) { c.Classification.Enabled = true }, []string{"classify"}),
		Entry("artifacts", func(c *config.AnalysisConfig) { c.Artifacts.Enabled = true }, []string{"artifacts"}),
		Entry("classify and embedding", func(c *config.AnalysisConfig) {
			c.Classification.Enabled, c.Embedding.Enabled = true, true
		}, []string{"classify", "embedding"}),
		Entry("classify, embedding and relationship", func(c *config.AnalysisConfig) {
			c.Classification.Enabled, c.Embedding.Enabled, c.Relationship.Enabled = true, true, true
		}, []string{"classify", "embedding", "relationship"}),
		Entry("classify, embedding and requires", func(c *config.AnalysisConfig) {
			c.Classification.Enabled, c.Embedding.Enabled, c.Requires.Enabled = true, true, true
		}, []string{"classify", "embedding", "requires"}),
		Entry("all five", func(c *config.AnalysisConfig) {
			c.Classification.Enabled, c.Embedding.Enabled, c.Artifacts.Enabled = true, true, true
			c.Requires.Enabled, c.Relationship.Enabled = true, true
		}, []string{"classify", "embedding", "artifacts", "requires", "relationship"}),
	)

	// Config validation rejects these combinations; the registry refuses them
	// too, so a caller that skips validation still fails at startup instead
	// of on the first job.
	DescribeTable("refuses an enabled analyzer whose dependency is disabled, naming both settings",
		func(disable func(*config.AnalysisConfig), msg ...string) {
			cfg := validAnalysisConfig()
			disable(&cfg)
			_, err := newProductionRegistryWithConfig(cfg, nil, nil)
			Expect(err).To(MatchError(analyzer.ErrMissingDependency))
			for _, m := range msg {
				Expect(err.Error()).To(ContainSubstring(m))
			}
		},
		Entry("embedding without classify", func(c *config.AnalysisConfig) {
			c.Classification.Enabled = false
		}, `analyzer "embedding" depends on "classify"`, "analysis.embedding.enabled", "analysis.classification.enabled"),
		Entry("relationship and requires without embedding", func(c *config.AnalysisConfig) {
			c.Embedding.Enabled = false
		}, `depends on "embedding"`, "analysis.embedding.enabled"),
	)

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
