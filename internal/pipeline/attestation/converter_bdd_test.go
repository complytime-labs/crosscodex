package attestation_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	pkgattestation "github.com/complytime-labs/crosscodex/pkg/attestation"

	pipelineattestation "github.com/complytime-labs/crosscodex/internal/pipeline/attestation"
)

func TestConverterBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Pipeline Attestation Converter BDD Suite")
}

var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

// stubAnalyzer implements analyzer.Analyzer[proto.Message] for testing.
type stubAnalyzer struct {
	name string
	deps []string
}

func (s *stubAnalyzer) Name() string        { return s.name }
func (s *stubAnalyzer) DependsOn() []string { return s.deps }
func (s *stubAnalyzer) GenerateWork(_ context.Context, _ proto.Message, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
	return nil, nil
}
func (s *stubAnalyzer) Aggregate(_ context.Context, _ []analyzer.TaskResult) (*analyzer.Output, error) {
	return nil, nil
}
func (s *stubAnalyzer) ResultSchema() proto.Message { return nil }

func buildDAG(analyzers ...*stubAnalyzer) *analyzer.DAG {
	reg := analyzer.NewRegistry()
	for _, a := range analyzers {
		err := analyzer.Register[proto.Message](reg, a)
		Expect(err).NotTo(HaveOccurred(), "register %s", a.name)
	}
	dag, err := reg.BuildDAG(context.Background())
	Expect(err).NotTo(HaveOccurred(), "build DAG")
	return dag
}

var _ = Describe("Converter", func() {
	DescribeTable("Convert",
		func(analyzers []*stubAnalyzer, wantSteps int, checkFunc func(pkgattestation.LayoutOptions)) {
			var dag *analyzer.DAG
			if len(analyzers) == 0 {
				reg := analyzer.NewRegistry()
				var err error
				dag, err = reg.BuildDAG(context.Background())
				Expect(err).NotTo(HaveOccurred(), "build empty DAG")
			} else {
				dag = buildDAG(analyzers...)
			}

			conv := pipelineattestation.NewConverter()
			opts := conv.Convert(dag)

			Expect(opts.Steps).To(HaveLen(wantSteps))

			if checkFunc != nil {
				checkFunc(opts)
			}
		},
		Entry("empty DAG produces zero steps",
			nil, 0, nil,
		),
		Entry("single analyzer with no deps",
			[]*stubAnalyzer{{name: "ingestion", deps: nil}},
			1,
			func(opts pkgattestation.LayoutOptions) {
				s := opts.Steps[0]
				Expect(s.Name).To(Equal("ingestion"))
				Expect(s.ExpectedMaterials).To(BeEmpty())
				Expect(s.ExpectedProducts).To(ConsistOf("ingestion.output"))
				Expect(s.Threshold).To(Equal(1))
			},
		),
		Entry("two analyzers with dependency",
			[]*stubAnalyzer{
				{name: "ingestion", deps: nil},
				{name: "analysis", deps: []string{"ingestion"}},
			},
			2,
			func(opts pkgattestation.LayoutOptions) {
				var analysisStep *pkgattestation.Step
				for i := range opts.Steps {
					if opts.Steps[i].Name == "analysis" {
						analysisStep = &opts.Steps[i]
						break
					}
				}
				Expect(analysisStep).NotTo(BeNil(), "analysis step not found")
				Expect(analysisStep.ExpectedMaterials).To(ConsistOf("ingestion.output"))
				Expect(analysisStep.ExpectedProducts).To(ConsistOf("analysis.output"))
			},
		),
		Entry("multi-level DAG preserves level ordering",
			[]*stubAnalyzer{
				{name: "ingestion", deps: nil},
				{name: "analysis", deps: []string{"ingestion"}},
				{name: "synthesis", deps: []string{"analysis"}},
			},
			3,
			func(opts pkgattestation.LayoutOptions) {
				names := make([]string, len(opts.Steps))
				for i, s := range opts.Steps {
					names[i] = s.Name
				}
				ingIdx, anaIdx, synIdx := -1, -1, -1
				for i, n := range names {
					switch n {
					case "ingestion":
						ingIdx = i
					case "analysis":
						anaIdx = i
					case "synthesis":
						synIdx = i
					}
				}
				Expect(ingIdx).To(BeNumerically("<", anaIdx))
				Expect(anaIdx).To(BeNumerically("<", synIdx))
			},
		),
		Entry("ExpiresIn is zero (caller responsibility)",
			[]*stubAnalyzer{{name: "step-a", deps: nil}},
			1,
			func(opts pkgattestation.LayoutOptions) {
				Expect(opts.ExpiresIn).To(BeZero())
			},
		),
	)
})
