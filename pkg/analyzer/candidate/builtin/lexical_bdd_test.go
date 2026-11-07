package builtin_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate/builtin"
)

var _ = Describe("LexicalGenerator", func() {
	var gen candidate.Generator

	textControls := func(texts map[string]string) map[string]*candidate.ControlData {
		controls := make(map[string]*candidate.ControlData, len(texts))
		for id, text := range texts {
			controls[id] = &candidate.ControlData{ControlID: id, Text: text}
		}
		return controls
	}

	policyTargets := func() map[string]*candidate.ControlData {
		return textControls(map[string]string{
			"T1": "access control policy",
			"T2": "audit policy",
			"T3": "policy review",
		})
	}

	generate := func(sourceText string, targets map[string]*candidate.ControlData, params map[string]interface{}) ([]candidate.Candidate, error) {
		return gen.Generate(context.Background(), candidate.GenerateRequest{
			SourceControls: textControls(map[string]string{"S": sourceText}),
			TargetControls: targets,
			Parameters:     params,
		})
	}

	BeforeEach(func() {
		gen = builtin.NewLexicalGenerator()
	})

	Describe("Name", func() {
		It("returns lexical", func() {
			Expect(gen.Name()).To(Equal("lexical"))
		})
	})

	Describe("Generate", func() {
		It("ranks the target sharing the rarer term first with a normalised score", func() {
			candidates, err := generate("access policy", policyTargets(), nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(candidates).To(HaveLen(3))

			Expect(candidates[0].TargetID).To(Equal("T1"))
			Expect(candidates[0].Score).To(Equal(1.0))
			for i, c := range candidates {
				Expect(c.GeneratorID).To(Equal("lexical"))
				Expect(c.SourceID).To(Equal("S"))
				Expect(c.Score).To(And(BeNumerically(">", 0), BeNumerically("<=", 1)))
				if i > 0 {
					Expect(c.Score).To(BeNumerically("<=", candidates[i-1].Score))
				}
			}
			Expect(candidates[1].Score).To(BeNumerically("<", 1.0))
		})

		It("keeps only top_k targets per source", func() {
			candidates, err := generate("access policy", policyTargets(), map[string]interface{}{"top_k": 1})
			Expect(err).NotTo(HaveOccurred())
			Expect(candidates).To(HaveLen(1))
			Expect(candidates[0].TargetID).To(Equal("T1"))
		})

		It("accepts top_k as a JSON float64", func() {
			candidates, err := generate("access policy", policyTargets(), map[string]interface{}{"top_k": float64(2)})
			Expect(err).NotTo(HaveOccurred())
			Expect(candidates).To(HaveLen(2))
		})

		It("applies the weight parameter", func() {
			candidates, err := generate("access policy", policyTargets(), map[string]interface{}{"weight": 2.5})
			Expect(err).NotTo(HaveOccurred())
			Expect(candidates).NotTo(BeEmpty())
			for _, c := range candidates {
				Expect(c.Weight).To(Equal(2.5))
			}
		})

		It("skips a control paired with itself", func() {
			controls := textControls(map[string]string{
				"A": "access control policy",
				"B": "access policy",
			})
			candidates, err := gen.Generate(context.Background(), candidate.GenerateRequest{
				SourceControls: controls, TargetControls: controls,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(candidates).To(HaveLen(2))
			for _, c := range candidates {
				Expect(c.SourceID).NotTo(Equal(c.TargetID))
			}
		})

		It("skips targets with no term overlap", func() {
			candidates, err := generate("encryption", policyTargets(), nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(candidates).To(BeEmpty())
		})

		It("orders equal scores by target ID ascending", func() {
			targets := textControls(map[string]string{"B2": "audit policy", "B1": "audit policy"})
			candidates, err := generate("audit", targets, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(candidates).To(HaveLen(2))
			Expect(candidates[0].TargetID).To(Equal("B1"))
			Expect(candidates[1].TargetID).To(Equal("B2"))
			Expect(candidates[0].Score).To(Equal(candidates[1].Score))
		})

		It("is deterministic across sources and runs", func() {
			sources := textControls(map[string]string{
				"S2": "access control policy review audit board",
				"S1": "access control policy review audit board",
				"S3": "audit policy review access",
			})
			targets := textControls(map[string]string{
				"T1": "access control policy",
				"T2": "audit policy review board of access",
				"T3": "policy review",
				"T4": "control audit board policy access review review",
			})
			req := candidate.GenerateRequest{SourceControls: sources, TargetControls: targets}
			first, err := gen.Generate(context.Background(), req)
			Expect(err).NotTo(HaveOccurred())
			Expect(first).NotTo(BeEmpty())
			for i := 0; i < 50; i++ {
				again, err := gen.Generate(context.Background(), req)
				Expect(err).NotTo(HaveOccurred())
				Expect(again).To(HaveLen(len(first)))
				for j := range first {
					Expect(again[j].SourceID).To(Equal(first[j].SourceID))
					Expect(again[j].TargetID).To(Equal(first[j].TargetID))
					Expect(again[j].Score).To(Equal(first[j].Score))
					Expect(again[j].Metadata["bm25"]).To(Equal(first[j].Metadata["bm25"]))
				}
			}
			Expect(first[0].SourceID).To(Equal("S1"))
		})

		It("ranks a short target above a long one containing the same term", func() {
			targets := textControls(map[string]string{
				"A-long":  "policy alpha beta gamma delta epsilon zeta eta theta",
				"B-short": "policy alpha",
			})
			candidates, err := generate("policy", targets, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(candidates).To(HaveLen(2))
			Expect(candidates[0].TargetID).To(Equal("B-short"))
			Expect(candidates[1].Score).To(BeNumerically("<", 1.0))
		})

		Context("tokenisation", func() {
			It("splits on non-alphanumerics and ignores case", func() {
				targets := textControls(map[string]string{"N1": "800 53 baseline", "N2": "80053"})
				candidates, err := generate("SP 800-53", targets, nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(candidates).To(HaveLen(1))
				Expect(candidates[0].TargetID).To(Equal("N1"))
			})

			It("folds case", func() {
				targets := textControls(map[string]string{"C1": "access control"})
				candidates, err := generate("ACCESS", targets, nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(candidates).To(HaveLen(1))
				Expect(candidates[0].TargetID).To(Equal("C1"))
			})

			It("produces no candidates for punctuation-only text", func() {
				candidates, err := generate("--- !!", policyTargets(), nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(candidates).To(BeEmpty())
			})
		})

		It("records bm25 metadata with four decimals", func() {
			candidates, err := generate("access policy", policyTargets(), nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(candidates).NotTo(BeEmpty())
			for _, c := range candidates {
				Expect(c.Metadata["bm25"]).To(MatchRegexp(`^\d+\.\d{4}$`))
			}
		})

		It("returns no candidates when there are no targets", func() {
			candidates, err := generate("access policy", map[string]*candidate.ControlData{}, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(candidates).To(BeEmpty())
		})

		DescribeTable("rejects invalid parameters",
			func(params map[string]interface{}, want string) {
				_, err := generate("access policy", policyTargets(), params)
				Expect(err).To(MatchError(ContainSubstring(want)))
			},
			Entry("top_k zero", map[string]interface{}{"top_k": 0}, "lexical: parameter top_k"),
			Entry("top_k negative", map[string]interface{}{"top_k": -1}, "lexical: parameter top_k"),
			Entry("top_k fractional", map[string]interface{}{"top_k": 1.5}, "lexical: parameter top_k"),
			Entry("top_k string", map[string]interface{}{"top_k": "3"}, "lexical: parameter top_k"),
			Entry("weight string", map[string]interface{}{"weight": "x"}, "lexical: parameter weight"),
		)

		It("works with telemetry configured", func() {
			g := builtin.NewLexicalGenerator(builtin.WithLexicalTelemetry(noop.NewTracerProvider().Tracer("test")))
			candidates, err := g.Generate(context.Background(), candidate.GenerateRequest{
				SourceControls: textControls(map[string]string{"S": "access policy"}),
				TargetControls: policyTargets(),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(candidates).NotTo(BeEmpty())
		})
	})
})
