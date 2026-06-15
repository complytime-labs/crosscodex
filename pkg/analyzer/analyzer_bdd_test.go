package analyzer_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	otelcodes "go.opentelemetry.io/otel/codes"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

func TestAnalyzerBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Analyzer BDD Suite")
}

// --- Mock analyzer for testing ---

// mockAnalyzer implements analyzer.Analyzer[T] for testing.
type mockAnalyzer[T proto.Message] struct {
	name      string
	deps      []string
	genWork   func(ctx context.Context, input T, config analyzer.AnalyzerConfig) ([]analyzer.Task, error)
	aggregate func(ctx context.Context, results []analyzer.TaskResult) (*analyzer.Output, error)
	schema    proto.Message
}

func (m *mockAnalyzer[T]) Name() string        { return m.name }
func (m *mockAnalyzer[T]) DependsOn() []string { return m.deps }
func (m *mockAnalyzer[T]) GenerateWork(ctx context.Context, input T, config analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
	if m.genWork != nil {
		return m.genWork(ctx, input, config)
	}
	return nil, nil
}
func (m *mockAnalyzer[T]) Aggregate(ctx context.Context, results []analyzer.TaskResult) (*analyzer.Output, error) {
	if m.aggregate != nil {
		return m.aggregate(ctx, results)
	}
	return &analyzer.Output{AnalyzerName: m.name}, nil
}
func (m *mockAnalyzer[T]) ResultSchema() proto.Message {
	if m.schema != nil {
		return m.schema
	}
	return &emptypb.Empty{}
}

// newMock creates a simple mock analyzer with the given name and dependencies.
func newMock(name string, deps ...string) *mockAnalyzer[*emptypb.Empty] {
	return &mockAnalyzer[*emptypb.Empty]{
		name: name,
		deps: deps,
	}
}

// registerBuiltinGraph registers the 5 built-in analyzer types into a registry.
// Returns the registry for further assertions.
//
//	Level 0: [artifacts, classify]     — no dependencies
//	Level 1: [embedding]               — depends on: classify
//	Level 2: [relationship, requires]  — depends on: embedding
func registerBuiltinGraph(r *analyzer.Registry) {
	Expect(analyzer.Register[*emptypb.Empty](r, newMock("artifacts"))).To(Succeed())
	Expect(analyzer.Register[*emptypb.Empty](r, newMock("classify"))).To(Succeed())
	Expect(analyzer.Register[*emptypb.Empty](r, newMock("embedding", "classify"))).To(Succeed())
	Expect(analyzer.Register[*emptypb.Empty](r, newMock("relationship", "embedding"))).To(Succeed())
	Expect(analyzer.Register[*emptypb.Empty](r, newMock("requires", "embedding"))).To(Succeed())
}

var restoreLogs func()

var _ = BeforeSuite(func() {
	restoreLogs = testspecs.RedirectLogsToGinkgo()
})

var _ = AfterSuite(func() {
	restoreLogs()
})

var _ = Describe("Analyzer Package", func() {

	// =================================================================
	// LEVEL 1: BEHAVIORAL SPECIFICATIONS
	// =================================================================

	Describe("DAG Construction from Built-in Analyzer Graph", func() {
		var (
			reg *analyzer.Registry
			dag *analyzer.DAG
		)

		BeforeEach(func() {
			reg = analyzer.NewRegistry()
			registerBuiltinGraph(reg)
			var err error
			dag, err = reg.BuildDAG(context.Background())
			Expect(err).NotTo(HaveOccurred())
		})

		It("produces three execution levels matching the built-in dependency graph", func() {
			levels := dag.Levels()
			Expect(levels).To(HaveLen(3))

			By("Level 0: artifacts and classify (no dependencies)")
			Expect(levels[0]).To(Equal([]string{"artifacts", "classify"}))

			By("Level 1: embedding (depends on classify)")
			Expect(levels[1]).To(Equal([]string{"embedding"}))

			By("Level 2: relationship and requires (depend on embedding)")
			Expect(levels[2]).To(Equal([]string{"relationship", "requires"}))
		})

		It("returns a deterministic flat topological order", func() {
			order1 := dag.Order()
			order2 := dag.Order()
			Expect(order1).To(Equal(order2))
			Expect(order1).To(Equal([]string{
				"artifacts", "classify", "embedding", "relationship", "requires",
			}))
		})

		It("returns analyzers in topological order", func() {
			analyzers := dag.Analyzers()
			Expect(analyzers).To(HaveLen(5))
			names := make([]string, len(analyzers))
			for i, a := range analyzers {
				names[i] = a.Name()
			}
			Expect(names).To(Equal([]string{
				"artifacts", "classify", "embedding", "relationship", "requires",
			}))
		})

		Context("Subset operations", func() {
			It("includes transitive dependencies for a leaf analyzer", func() {
				By("Subset(relationship) should include classify, embedding, relationship")
				sub, err := dag.Subset("relationship")
				Expect(err).NotTo(HaveOccurred())

				levels := sub.Levels()
				Expect(levels).To(HaveLen(3))
				Expect(levels[0]).To(Equal([]string{"classify"}))
				Expect(levels[1]).To(Equal([]string{"embedding"}))
				Expect(levels[2]).To(Equal([]string{"relationship"}))
			})

			It("returns only the target when it has no dependencies", func() {
				By("Subset(artifacts) should return only artifacts")
				sub, err := dag.Subset("artifacts")
				Expect(err).NotTo(HaveOccurred())

				levels := sub.Levels()
				Expect(levels).To(HaveLen(1))
				Expect(levels[0]).To(Equal([]string{"artifacts"}))
			})

			It("merges overlapping dependency trees", func() {
				By("Subset(relationship, artifacts) should include both trees")
				sub, err := dag.Subset("relationship", "artifacts")
				Expect(err).NotTo(HaveOccurred())

				levels := sub.Levels()
				Expect(levels).To(HaveLen(3))
				Expect(levels[0]).To(ConsistOf("artifacts", "classify"))
				Expect(levels[1]).To(Equal([]string{"embedding"}))
				Expect(levels[2]).To(Equal([]string{"relationship"}))
			})

			It("deduplicates identical transitive dependency chains", func() {
				By("Subset(relationship, requires) — both share classify -> embedding")
				sub, err := dag.Subset("relationship", "requires")
				Expect(err).NotTo(HaveOccurred())

				levels := sub.Levels()
				Expect(levels).To(HaveLen(3))
				Expect(levels[0]).To(Equal([]string{"classify"}))
				Expect(levels[1]).To(Equal([]string{"embedding"}))
				Expect(levels[2]).To(ConsistOf("relationship", "requires"))
			})
		})
	})

	Describe("Registry Operations", func() {
		var reg *analyzer.Registry

		BeforeEach(func() {
			reg = analyzer.NewRegistry()
		})

		It("retrieves a registered analyzer by name", func() {
			Expect(analyzer.Register[*emptypb.Empty](reg, newMock("classify"))).To(Succeed())

			a, err := reg.Get("classify")
			Expect(err).NotTo(HaveOccurred())
			Expect(a.Name()).To(Equal("classify"))
		})

		It("lists all registered analyzers", func() {
			registerBuiltinGraph(reg)
			all := reg.All()
			Expect(all).To(HaveLen(5))

			names := make([]string, len(all))
			for i, a := range all {
				names[i] = a.Name()
			}
			Expect(names).To(ContainElements("artifacts", "classify", "embedding", "relationship", "requires"))
		})
	})

	Describe("Type-Safe Registration Bridge", func() {
		var reg *analyzer.Registry

		BeforeEach(func() {
			reg = analyzer.NewRegistry()
		})

		It("passes through GenerateWork with correct proto type", func() {
			called := false
			mock := &mockAnalyzer[*wrapperspb.StringValue]{
				name: "typed",
				genWork: func(_ context.Context, input *wrapperspb.StringValue, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
					Expect(input.GetValue()).To(Equal("test-control"))
					called = true
					return []analyzer.Task{{TaskID: "t1", TaskType: "typed"}}, nil
				},
			}
			Expect(analyzer.Register[*wrapperspb.StringValue](reg, mock)).To(Succeed())

			a, err := reg.Get("typed")
			Expect(err).NotTo(HaveOccurred())

			tasks, err := a.GenerateWorkFromProto(
				context.Background(),
				wrapperspb.String("test-control"),
				analyzer.AnalyzerConfig{},
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(tasks).To(HaveLen(1))
			Expect(called).To(BeTrue())
		})

		It("reports InputType as the proto full name", func() {
			mock := &mockAnalyzer[*wrapperspb.StringValue]{name: "typed"}
			Expect(analyzer.Register[*wrapperspb.StringValue](reg, mock)).To(Succeed())

			a, err := reg.Get("typed")
			Expect(err).NotTo(HaveOccurred())
			Expect(a.InputType()).To(Equal("google.protobuf.StringValue"))
		})

		It("delegates Aggregate through the registered wrapper", func() {
			mergedResult := wrapperspb.String("merged-output")
			expectedOutput := &analyzer.Output{
				AnalyzerName: "aggregator",
				Data:         mergedResult,
			}
			mock := &mockAnalyzer[*emptypb.Empty]{
				name: "aggregator",
				aggregate: func(_ context.Context, results []analyzer.TaskResult) (*analyzer.Output, error) {
					Expect(results).To(HaveLen(2))
					Expect(results[0].TaskID).To(Equal("t1"))
					Expect(results[1].TaskID).To(Equal("t2"))
					return expectedOutput, nil
				},
			}
			Expect(analyzer.Register[*emptypb.Empty](reg, mock)).To(Succeed())

			a, err := reg.Get("aggregator")
			Expect(err).NotTo(HaveOccurred())

			results := []analyzer.TaskResult{
				{TaskID: "t1", Result: wrapperspb.String("score-0.9")},
				{TaskID: "t2", Result: wrapperspb.String("score-0.8")},
			}
			output, err := a.Aggregate(context.Background(), results)
			Expect(err).NotTo(HaveOccurred())
			Expect(output.AnalyzerName).To(Equal("aggregator"))
			Expect(output.Data).To(Equal(mergedResult))
		})

		It("delegates ResultSchema through the registered wrapper", func() {
			expectedSchema := wrapperspb.String("schema-value")
			mock := &mockAnalyzer[*wrapperspb.StringValue]{
				name:   "with-schema",
				schema: expectedSchema,
			}
			Expect(analyzer.Register[*wrapperspb.StringValue](reg, mock)).To(Succeed())

			a, err := reg.Get("with-schema")
			Expect(err).NotTo(HaveOccurred())

			schema := a.ResultSchema()
			Expect(schema).NotTo(BeNil())

			sv, ok := schema.(*wrapperspb.StringValue)
			Expect(ok).To(BeTrue(), "expected ResultSchema to return *wrapperspb.StringValue")
			Expect(sv.GetValue()).To(Equal("schema-value"))
		})
	})

	// =================================================================
	// LEVEL 2: TECHNICAL EDGE CASES AND ERROR PATHS
	// =================================================================

	Describe("Error Sentinels", func() {
		It("provides distinct error identities for each failure mode", func() {
			sentinels := []error{
				analyzer.ErrNotFound,
				analyzer.ErrAlreadyRegistered,
				analyzer.ErrCycleDetected,
				analyzer.ErrMissingDependency,
			}

			By("ensuring every sentinel is non-nil")
			for _, s := range sentinels {
				Expect(s).NotTo(BeNil())
			}

			By("ensuring no two sentinels are identical")
			for i := 0; i < len(sentinels); i++ {
				for j := i + 1; j < len(sentinels); j++ {
					Expect(errors.Is(sentinels[i], sentinels[j])).To(BeFalse(),
						"sentinel %d and %d should be distinct", i, j)
				}
			}
		})

		It("supports error wrapping for contextual diagnostics", func() {
			wrapped := fmt.Errorf("lookup failed: %w", analyzer.ErrNotFound)
			Expect(errors.Is(wrapped, analyzer.ErrNotFound)).To(BeTrue())

			wrapped = fmt.Errorf("registration rejected: %w", analyzer.ErrAlreadyRegistered)
			Expect(errors.Is(wrapped, analyzer.ErrAlreadyRegistered)).To(BeTrue())

			wrapped = fmt.Errorf("graph invalid: %w", analyzer.ErrCycleDetected)
			Expect(errors.Is(wrapped, analyzer.ErrCycleDetected)).To(BeTrue())

			wrapped = fmt.Errorf("dep missing: %w", analyzer.ErrMissingDependency)
			Expect(errors.Is(wrapped, analyzer.ErrMissingDependency)).To(BeTrue())
		})
	})

	Describe("Cycle Detection", func() {
		var reg *analyzer.Registry

		BeforeEach(func() {
			reg = analyzer.NewRegistry()
		})

		It("detects a simple A -> B -> A cycle", func() {
			Expect(analyzer.Register[*emptypb.Empty](reg, newMock("a", "b"))).To(Succeed())
			Expect(analyzer.Register[*emptypb.Empty](reg, newMock("b", "a"))).To(Succeed())

			_, err := reg.BuildDAG(context.Background())
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, analyzer.ErrCycleDetected)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring("->"))
		})

		It("detects a three-node A -> B -> C -> A cycle", func() {
			Expect(analyzer.Register[*emptypb.Empty](reg, newMock("a", "b"))).To(Succeed())
			Expect(analyzer.Register[*emptypb.Empty](reg, newMock("b", "c"))).To(Succeed())
			Expect(analyzer.Register[*emptypb.Empty](reg, newMock("c", "a"))).To(Succeed())

			_, err := reg.BuildDAG(context.Background())
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, analyzer.ErrCycleDetected)).To(BeTrue())
		})

		It("detects self-referential dependencies", func() {
			Expect(analyzer.Register[*emptypb.Empty](reg, newMock("self", "self"))).To(Succeed())

			_, err := reg.BuildDAG(context.Background())
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, analyzer.ErrCycleDetected)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring("self"))
		})
	})

	Describe("Missing Dependency Detection", func() {
		It("returns ErrMissingDependency with both names in the message", func() {
			reg := analyzer.NewRegistry()
			Expect(analyzer.Register[*emptypb.Empty](reg, newMock("relationship", "embedding"))).To(Succeed())

			_, err := reg.BuildDAG(context.Background())
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, analyzer.ErrMissingDependency)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring("relationship"))
			Expect(err.Error()).To(ContainSubstring("embedding"))
			Expect(err.Error()).To(ContainSubstring("not registered"))
		})
	})

	Describe("Duplicate Registration", func() {
		It("returns ErrAlreadyRegistered with the analyzer name", func() {
			reg := analyzer.NewRegistry()
			Expect(analyzer.Register[*emptypb.Empty](reg, newMock("classify"))).To(Succeed())

			err := analyzer.Register[*emptypb.Empty](reg, newMock("classify"))
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, analyzer.ErrAlreadyRegistered)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring("classify"))
		})
	})

	Describe("Name Validation", func() {
		var reg *analyzer.Registry

		BeforeEach(func() {
			reg = analyzer.NewRegistry()
		})

		It("accepts valid names", func() {
			validNames := []string{"a", "classify", "embedding-v2", "my-analyzer", "a1b", "abc123"}
			for _, name := range validNames {
				err := analyzer.Register[*emptypb.Empty](reg, newMock(name))
				Expect(err).NotTo(HaveOccurred(), "expected %q to be valid", name)
			}
		})

		It("accepts names with underscores", func() {
			Expect(analyzer.Register[*emptypb.Empty](reg, newMock("my_analyzer"))).To(Succeed())
		})

		It("rejects empty names", func() {
			err := analyzer.Register[*emptypb.Empty](reg, newMock(""))
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, analyzer.ErrInvalidName)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring("must not be empty"))
		})

		It("rejects names starting with a digit", func() {
			err := analyzer.Register[*emptypb.Empty](reg, newMock("1abc"))
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, analyzer.ErrInvalidName)).To(BeTrue())
		})

		It("rejects names with uppercase letters", func() {
			err := analyzer.Register[*emptypb.Empty](reg, newMock("MyAnalyzer"))
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, analyzer.ErrInvalidName)).To(BeTrue())
		})

		It("rejects names with special characters", func() {
			badNames := []string{"my.analyzer", "my@analyzer", "my analyzer", "my>analyzer", "my*analyzer"}
			for _, name := range badNames {
				err := analyzer.Register[*emptypb.Empty](reg, newMock(name))
				Expect(err).To(HaveOccurred(), "expected %q to be rejected", name)
				Expect(errors.Is(err, analyzer.ErrInvalidName)).To(BeTrue())
			}
		})

		It("rejects names ending with a hyphen", func() {
			err := analyzer.Register[*emptypb.Empty](reg, newMock("abc-"))
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, analyzer.ErrInvalidName)).To(BeTrue())
		})

		It("rejects names starting with a hyphen", func() {
			err := analyzer.Register[*emptypb.Empty](reg, newMock("-abc"))
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, analyzer.ErrInvalidName)).To(BeTrue())
		})

		It("rejects names exceeding 64 characters", func() {
			// 65 lowercase letters: starts with 'a', rest 'b's
			name := "a" + strings.Repeat("b", 64)
			Expect(name).To(HaveLen(65))
			err := analyzer.Register[*emptypb.Empty](reg, newMock(name))
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, analyzer.ErrInvalidName)).To(BeTrue())
		})

		It("accepts names at the maximum length of 64 characters", func() {
			name := "a" + strings.Repeat("b", 63)
			Expect(name).To(HaveLen(64))
			Expect(analyzer.Register[*emptypb.Empty](reg, newMock(name))).To(Succeed())
		})

		It("includes the invalid name and pattern in the error message", func() {
			err := analyzer.Register[*emptypb.Empty](reg, newMock("BAD!"))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("BAD!"))
			Expect(err.Error()).To(ContainSubstring("pattern"))
		})

		It("validates dependency names are not checked during registration", func() {
			By("dependency names are validated during BuildDAG, not Register")
			Expect(analyzer.Register[*emptypb.Empty](reg, newMock("valid", "ANY-DEP"))).To(Succeed())
		})
	})

	Describe("Empty Registry", func() {
		It("builds a DAG with no levels", func() {
			reg := analyzer.NewRegistry()
			dag, err := reg.BuildDAG(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(dag.Levels()).To(BeEmpty())
			Expect(dag.Order()).To(BeEmpty())
			Expect(dag.Analyzers()).To(BeEmpty())
		})
	})

	Describe("Single Analyzer", func() {
		It("produces one level with one analyzer", func() {
			reg := analyzer.NewRegistry()
			Expect(analyzer.Register[*emptypb.Empty](reg, newMock("solo"))).To(Succeed())

			dag, err := reg.BuildDAG(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(dag.Levels()).To(HaveLen(1))
			Expect(dag.Levels()[0]).To(Equal([]string{"solo"}))
		})
	})

	Describe("Subset Error Handling", func() {
		It("returns ErrNotFound for unknown analyzer names", func() {
			reg := analyzer.NewRegistry()
			registerBuiltinGraph(reg)
			dag, err := reg.BuildDAG(context.Background())
			Expect(err).NotTo(HaveOccurred())

			_, err = dag.Subset("nonexistent")
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, analyzer.ErrNotFound)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring("nonexistent"))
		})
	})

	Describe("Get Error Handling", func() {
		It("returns ErrNotFound for unregistered analyzer", func() {
			reg := analyzer.NewRegistry()
			_, err := reg.Get("missing")
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, analyzer.ErrNotFound)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring("missing"))
		})
	})

	Describe("Wrong Proto Type Handling", func() {
		It("returns an actionable error when GenerateWorkFromProto receives wrong type", func() {
			reg := analyzer.NewRegistry()
			mock := &mockAnalyzer[*wrapperspb.StringValue]{name: "typed"}
			Expect(analyzer.Register[*wrapperspb.StringValue](reg, mock)).To(Succeed())

			a, err := reg.Get("typed")
			Expect(err).NotTo(HaveOccurred())

			By("passing a Struct instead of the expected StringValue")
			_, err = a.GenerateWorkFromProto(
				context.Background(),
				&structpb.Struct{},
				analyzer.AnalyzerConfig{},
			)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("typed"))
			Expect(err.Error()).To(ContainSubstring("StringValue"))
		})
	})

	Describe("Telemetry Integration", func() {
		It("produces a span on BuildDAG", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = tp.Shutdown(context.Background()) }()

			reg := analyzer.NewRegistry(
				analyzer.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()),
			)
			registerBuiltinGraph(reg)

			dag, err := reg.BuildDAG(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(dag).NotTo(BeNil())

			spans := tp.GetSpans()
			span := telemetrytest.FindSpan(spans, "analyzer.BuildDAG")
			Expect(span).NotTo(BeNil(), "expected a span named analyzer.BuildDAG")

			_, found := telemetrytest.SpanAttribute(span, "analyzer.count")
			Expect(found).To(BeTrue(), "expected analyzer.count attribute")

			_, found = telemetrytest.SpanAttribute(span, "dag.levels")
			Expect(found).To(BeTrue(), "expected dag.levels attribute")
		})

		It("records registration counter and analyzer gauge metrics", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = tp.Shutdown(context.Background()) }()

			reg := analyzer.NewRegistry(
				analyzer.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()),
			)

			Expect(analyzer.Register[*emptypb.Empty](reg, newMock("alpha"))).To(Succeed())
			Expect(analyzer.Register[*emptypb.Empty](reg, newMock("beta"))).To(Succeed())
			Expect(analyzer.Register[*emptypb.Empty](reg, newMock("gamma"))).To(Succeed())

			rm := tp.GetMetrics()

			By("checking registration counter")
			counterMetric := telemetrytest.FindMetric(rm, "analyzer.registrations.total")
			Expect(counterMetric).NotTo(BeNil(), "expected analyzer.registrations.total metric")
			count, err := telemetrytest.CounterValue(counterMetric)
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(3)))

			By("checking analyzer gauge")
			gaugeMetric := telemetrytest.FindMetric(rm, "analyzer.registered.count")
			Expect(gaugeMetric).NotTo(BeNil(), "expected analyzer.registered.count metric")
			gaugeVal, err := telemetrytest.GaugeValue(gaugeMetric)
			Expect(err).NotTo(HaveOccurred())
			Expect(gaugeVal).To(Equal(int64(3)))
		})

		It("records BuildDAG duration histogram", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = tp.Shutdown(context.Background()) }()

			reg := analyzer.NewRegistry(
				analyzer.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()),
			)
			registerBuiltinGraph(reg)

			_, err = reg.BuildDAG(context.Background())
			Expect(err).NotTo(HaveOccurred())

			rm := tp.GetMetrics()
			histMetric := telemetrytest.FindMetric(rm, "analyzer.build_dag.duration_ms")
			Expect(histMetric).NotTo(BeNil(), "expected analyzer.build_dag.duration_ms metric")
		})

		It("records error status on span when BuildDAG fails", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = tp.Shutdown(context.Background()) }()

			reg := analyzer.NewRegistry(
				analyzer.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()),
			)
			// Create a cycle: a -> b -> a
			Expect(analyzer.Register[*emptypb.Empty](reg, newMock("a", "b"))).To(Succeed())
			Expect(analyzer.Register[*emptypb.Empty](reg, newMock("b", "a"))).To(Succeed())

			_, err = reg.BuildDAG(context.Background())
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, analyzer.ErrCycleDetected)).To(BeTrue())

			spans := tp.GetSpans()
			span := telemetrytest.FindSpan(spans, "analyzer.BuildDAG")
			Expect(span).NotTo(BeNil(), "expected a span named analyzer.BuildDAG on error path")
			Expect(span.Status().Code).To(Equal(otelcodes.Error))
			Expect(span.Status().Description).To(ContainSubstring("cycle"))
		})
	})

	Describe("Levels and Order Return Copies", func() {
		It("does not allow external mutation of DAG internals", func() {
			reg := analyzer.NewRegistry()
			registerBuiltinGraph(reg)
			dag, err := reg.BuildDAG(context.Background())
			Expect(err).NotTo(HaveOccurred())

			By("mutating the returned levels slice")
			levels := dag.Levels()
			levels[0][0] = "MUTATED"

			By("verifying the original is unchanged")
			freshLevels := dag.Levels()
			Expect(freshLevels[0][0]).NotTo(Equal("MUTATED"))
			Expect(freshLevels[0]).To(Equal([]string{"artifacts", "classify"}))
		})

		It("does not allow external mutation of Order internals", func() {
			reg := analyzer.NewRegistry()
			registerBuiltinGraph(reg)
			dag, err := reg.BuildDAG(context.Background())
			Expect(err).NotTo(HaveOccurred())

			By("mutating the returned order slice")
			order := dag.Order()
			order[0] = "MUTATED"

			By("verifying the original is unchanged")
			freshOrder := dag.Order()
			Expect(freshOrder[0]).NotTo(Equal("MUTATED"))
			Expect(freshOrder[0]).To(Equal("artifacts"))
		})

		It("does not allow external mutation of Analyzers internals", func() {
			reg := analyzer.NewRegistry()
			registerBuiltinGraph(reg)
			dag, err := reg.BuildDAG(context.Background())
			Expect(err).NotTo(HaveOccurred())

			By("mutating the returned analyzers slice")
			analyzers := dag.Analyzers()
			analyzers[0] = nil

			By("verifying the original is unchanged")
			freshAnalyzers := dag.Analyzers()
			Expect(freshAnalyzers[0]).NotTo(BeNil())
			Expect(freshAnalyzers[0].Name()).To(Equal("artifacts"))
		})

		It("does not allow external mutation of DependsOn return", func() {
			reg := analyzer.NewRegistry()
			Expect(analyzer.Register[*emptypb.Empty](reg, newMock("leaf", "dep-a", "dep-b"))).To(Succeed())

			a, err := reg.Get("leaf")
			Expect(err).NotTo(HaveOccurred())

			By("mutating the returned deps slice")
			deps := a.DependsOn()
			Expect(deps).To(Equal([]string{"dep-a", "dep-b"}))
			deps[0] = "MUTATED"

			By("verifying the original is unchanged")
			freshDeps := a.DependsOn()
			Expect(freshDeps[0]).To(Equal("dep-a"))
		})
	})

	// =================================================================
	// LEVEL 3: CONCURRENCY
	// =================================================================

	Describe("Concurrent Registry Access", func() {
		It("handles concurrent Get calls during Register without data races", func() {
			reg := analyzer.NewRegistry()

			var wg sync.WaitGroup
			const numWriters = 10
			const numReaders = 20

			// Start barrier ensures all goroutines launch before any begin work.
			start := make(chan struct{})

			// Start writers.
			for i := 0; i < numWriters; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					name := fmt.Sprintf("analyzer-%d", i)
					_ = analyzer.Register[*emptypb.Empty](reg, newMock(name))
				}(i)
			}

			// Start readers.
			for i := 0; i < numReaders; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					name := fmt.Sprintf("analyzer-%d", i%numWriters)
					// May or may not find the analyzer depending on timing.
					_, _ = reg.Get(name)
					_ = reg.All()
				}(i)
			}

			// Release all goroutines simultaneously.
			close(start)
			wg.Wait()

			// All writers used unique names, so all should have registered.
			all := reg.All()
			Expect(all).To(HaveLen(numWriters))
		})
	})
})
