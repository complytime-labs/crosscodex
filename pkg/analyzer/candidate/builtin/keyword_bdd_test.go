package builtin_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate/builtin"
)

var _ = Describe("KeywordGenerator", func() {
	var gen candidate.Generator

	BeforeEach(func() {
		gen = builtin.NewKeywordGenerator()
	})

	Describe("Name", func() {
		It("returns keyword", func() {
			Expect(gen.Name()).To(Equal("keyword"))
		})
	})

	Describe("Generate", func() {
		Context("when targets contain foundational keywords", func() {
			It("pairs every source with every matching target", func() {
				sources := map[string]*candidate.ControlData{
					"AC-1": {ControlID: "AC-1", Text: "Monitor access to systems"},
					"AC-2": {ControlID: "AC-2", Text: "Enforce access policies"},
				}

				targets := map[string]*candidate.ControlData{
					"AC-10": {
						ControlID: "AC-10",
						Text:      "The organization shall define and document access control policy",
					},
					"AC-11": {
						ControlID: "AC-11",
						Text:      "Establish a baseline for system configuration",
					},
					"AC-12": {
						ControlID: "AC-12",
						Text:      "Review access logs quarterly",
					},
				}

				req := candidate.GenerateRequest{
					TenantID:       "test-tenant",
					JobID:          "test-job",
					SourceControls: sources,
					TargetControls: targets,
					Parameters: map[string]interface{}{
						"keywords": []interface{}{"policy", "baseline", "define", "establish"},
						"weight":   0.8,
					},
				}

				candidates, err := gen.Generate(context.Background(), req)
				Expect(err).NotTo(HaveOccurred())

				// Should find AC-10 (contains "define", "policy") and AC-11 (contains "establish", "baseline")
				// Each source should be paired with each foundational target
				// 2 sources x 2 foundational targets = 4 candidates
				Expect(candidates).To(HaveLen(4))

				hasPair := func(sourceID, targetID string) bool {
					for _, c := range candidates {
						if c.SourceID == sourceID && c.TargetID == targetID {
							return true
						}
					}
					return false
				}

				Expect(hasPair("AC-1", "AC-10")).To(BeTrue())
				Expect(hasPair("AC-1", "AC-11")).To(BeTrue())
				Expect(hasPair("AC-2", "AC-10")).To(BeTrue())
				Expect(hasPair("AC-2", "AC-11")).To(BeTrue())

				// Verify metadata and scoring
				for _, c := range candidates {
					Expect(c.Score).To(Equal(1.0), "Keyword match should have score 1.0")
					Expect(c.Weight).To(Equal(0.8))
					Expect(c.GeneratorID).To(Equal("keyword"))
					Expect(c.Metadata).To(HaveKey("keywords_matched"))
				}
			})
		})

		Context("when keywords are case insensitive", func() {
			It("matches regardless of case", func() {
				sources := map[string]*candidate.ControlData{
					"AC-1": {ControlID: "AC-1", Text: "Some control"},
				}

				targets := map[string]*candidate.ControlData{
					"AC-10": {
						ControlID: "AC-10",
						Text:      "The organization shall DEFINE and document POLICY",
					},
				}

				req := candidate.GenerateRequest{
					TenantID:       "test-tenant",
					JobID:          "test-job",
					SourceControls: sources,
					TargetControls: targets,
					Parameters: map[string]interface{}{
						"keywords": []interface{}{"policy", "define"},
						"weight":   1.0,
					},
				}

				candidates, err := gen.Generate(context.Background(), req)
				Expect(err).NotTo(HaveOccurred())

				Expect(candidates).To(HaveLen(1))
				Expect(candidates[0].SourceID).To(Equal("AC-1"))
				Expect(candidates[0].TargetID).To(Equal("AC-10"))
			})
		})

		Context("when no keywords are found in targets", func() {
			It("returns an empty slice", func() {
				sources := map[string]*candidate.ControlData{
					"AC-1": {ControlID: "AC-1", Text: "Some control"},
				}

				targets := map[string]*candidate.ControlData{
					"AC-10": {
						ControlID: "AC-10",
						Text:      "This target has no foundational keywords",
					},
				}

				req := candidate.GenerateRequest{
					TenantID:       "test-tenant",
					JobID:          "test-job",
					SourceControls: sources,
					TargetControls: targets,
					Parameters: map[string]interface{}{
						"keywords": []interface{}{"policy", "procedure", "baseline"},
						"weight":   1.0,
					},
				}

				candidates, err := gen.Generate(context.Background(), req)
				Expect(err).NotTo(HaveOccurred())
				Expect(candidates).To(BeEmpty(), "Should return empty when no targets match keywords")
			})
		})

		Context("when no keywords parameter is provided", func() {
			It("returns an empty slice", func() {
				req := candidate.GenerateRequest{
					TenantID:       "test-tenant",
					JobID:          "test-job",
					SourceControls: map[string]*candidate.ControlData{},
					TargetControls: map[string]*candidate.ControlData{},
					Parameters:     map[string]interface{}{}, // No keywords parameter
				}

				candidates, err := gen.Generate(context.Background(), req)
				Expect(err).NotTo(HaveOccurred())
				Expect(candidates).To(BeEmpty(), "Should return empty when no keywords configured")
			})
		})

		Context("when a keyword is a substring of a target word", func() {
			It("matches partial keywords", func() {
				sources := map[string]*candidate.ControlData{
					"AC-1": {ControlID: "AC-1", Text: "Source control"},
				}

				targets := map[string]*candidate.ControlData{
					"AC-10": {
						ControlID: "AC-10",
						Text:      "Policies and procedures for access control", // Contains "procedures" (plural)
					},
				}

				req := candidate.GenerateRequest{
					TenantID:       "test-tenant",
					JobID:          "test-job",
					SourceControls: sources,
					TargetControls: targets,
					Parameters: map[string]interface{}{
						"keywords": []interface{}{"procedure"}, // Singular form
						"weight":   1.0,
					},
				}

				candidates, err := gen.Generate(context.Background(), req)
				Expect(err).NotTo(HaveOccurred())

				// Should match "procedures" when searching for "procedure"
				Expect(candidates).To(HaveLen(1))
				Expect(candidates[0].TargetID).To(Equal("AC-10"))
			})
		})

		Context("when using default keywords", func() {
			It("uses built-in defaults when no keywords parameter is set", func() {
				sources := map[string]*candidate.ControlData{
					"AC-1": {ControlID: "AC-1", Text: "Source"},
				}

				targets := map[string]*candidate.ControlData{
					"AC-10": {ControlID: "AC-10", Text: "Define the security policy"},
					"AC-11": {ControlID: "AC-11", Text: "Review access logs"},
				}

				req := candidate.GenerateRequest{
					TenantID:       "test-tenant",
					JobID:          "test-job",
					SourceControls: sources,
					TargetControls: targets,
					Parameters: map[string]interface{}{
						"weight": 1.0,
						// No keywords parameter - should use defaults
					},
				}

				candidates, err := gen.Generate(context.Background(), req)
				Expect(err).NotTo(HaveOccurred())

				// Should use default keywords and find AC-10 (contains "policy" and "define")
				Expect(candidates).To(HaveLen(1))
				Expect(candidates[0].TargetID).To(Equal("AC-10"))
			})
		})

		Context("when telemetry is configured", func() {
			It("generates candidates with a noop tracer", func() {
				tracer := noop.NewTracerProvider().Tracer("test")
				genWithTelemetry := builtin.NewKeywordGenerator(builtin.WithKeywordTelemetry(tracer))

				sources := map[string]*candidate.ControlData{
					"SRC-1": {ControlID: "SRC-1", Text: "Any source"},
				}

				targets := map[string]*candidate.ControlData{
					"TGT-1": {ControlID: "TGT-1", Text: "Establish a policy"},
				}

				req := candidate.GenerateRequest{
					TenantID:       "test-tenant",
					SourceControls: sources,
					TargetControls: targets,
					Parameters:     map[string]interface{}{},
				}

				candidates, err := genWithTelemetry.Generate(context.Background(), req)
				Expect(err).NotTo(HaveOccurred())
				Expect(candidates).NotTo(BeEmpty())
			})
		})
	})
})
