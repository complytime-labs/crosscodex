package attestation_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/analyzertest"
	pkgattestation "github.com/complytime-labs/crosscodex/pkg/attestation"

	pipelineattestation "github.com/complytime-labs/crosscodex/internal/pipeline/attestation"
)

func TestConverterBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Pipeline Attestation Converter BDD Suite")
}

var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

func buildDAG(analyzers ...*analyzertest.Stub[proto.Message]) *analyzer.DAG {
	reg := analyzer.NewRegistry()
	for _, a := range analyzers {
		err := analyzer.Register[proto.Message](reg, a)
		Expect(err).NotTo(HaveOccurred(), "register %s", a.AnalyzerName)
	}
	dag, err := reg.BuildDAG(context.Background())
	Expect(err).NotTo(HaveOccurred(), "build DAG")
	return dag
}

var _ = Describe("Converter", func() {
	DescribeTable("Convert",
		func(analyzers []*analyzertest.Stub[proto.Message], wantSteps int, checkFunc func(pkgattestation.LayoutOptions)) {
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
			[]*analyzertest.Stub[proto.Message]{{AnalyzerName: "ingestion"}},
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
			[]*analyzertest.Stub[proto.Message]{
				{AnalyzerName: "ingestion"},
				{AnalyzerName: "analysis", Dependencies: []string{"ingestion"}},
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
			[]*analyzertest.Stub[proto.Message]{
				{AnalyzerName: "ingestion"},
				{AnalyzerName: "analysis", Dependencies: []string{"ingestion"}},
				{AnalyzerName: "synthesis", Dependencies: []string{"analysis"}},
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
			[]*analyzertest.Stub[proto.Message]{{AnalyzerName: "step-a"}},
			1,
			func(opts pkgattestation.LayoutOptions) {
				Expect(opts.ExpiresIn).To(BeZero())
			},
		),
	)
})
