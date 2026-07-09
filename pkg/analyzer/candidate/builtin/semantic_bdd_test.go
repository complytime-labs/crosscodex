package builtin_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate/builtin"
)

// SimilarityMatrix mirrors internal/analyzer/embedding/types.go for testing.
type SimilarityMatrix struct {
	IDs    []string
	Values [][]float32
}

var _ = Describe("SemanticGenerator", func() {
	var gen candidate.Generator

	BeforeEach(func() {
		gen = builtin.NewSemanticGenerator()
	})

	Describe("Name", func() {
		It("returns semantic", func() {
			Expect(gen.Name()).To(Equal("semantic"))
		})
	})

	Describe("Generate", func() {
		Context("when selecting top-K similar targets", func() {
			It("returns the top-K most similar targets sorted by score", func() {
				// Create a 3x3 similarity matrix
				// AC-1: [100, 80, 60]
				// AC-2: [80, 100, 70]
				// AC-3: [60, 70, 100]
				matrix := &SimilarityMatrix{
					IDs: []string{"AC-1", "AC-2", "AC-3"},
					Values: [][]float32{
						{100, 80, 60},
						{80, 100, 70},
						{60, 70, 100},
					},
				}

				sources := map[string]*candidate.ControlData{
					"AC-1": {ControlID: "AC-1", Text: "Source control 1"},
				}

				targets := map[string]*candidate.ControlData{
					"AC-2": {ControlID: "AC-2", Text: "Target control 2"},
					"AC-3": {ControlID: "AC-3", Text: "Target control 3"},
				}

				req := candidate.GenerateRequest{
					TenantID:        "test-tenant",
					JobID:           "test-job",
					SourceControls:  sources,
					TargetControls:  targets,
					EmbeddingMatrix: matrix,
					Parameters: map[string]interface{}{
						"top_k":          2,
						"min_similarity": 60.0,
						"weight":         1.0,
					},
				}

				candidates, err := gen.Generate(context.Background(), req)
				Expect(err).NotTo(HaveOccurred())

				// Should return top-2 most similar targets for AC-1
				// AC-2 (similarity 80) and AC-3 (similarity 60)
				Expect(candidates).To(HaveLen(2))

				// Sort by score descending
				if candidates[0].Score < candidates[1].Score {
					candidates[0], candidates[1] = candidates[1], candidates[0]
				}

				// First should be AC-2 with score 0.80 (80/100)
				Expect(candidates[0].SourceID).To(Equal("AC-1"))
				Expect(candidates[0].TargetID).To(Equal("AC-2"))
				Expect(candidates[0].Score).To(BeNumerically("~", 0.80, 0.01))
				Expect(candidates[0].Weight).To(Equal(1.0))
				Expect(candidates[0].GeneratorID).To(Equal("semantic"))

				// Second should be AC-3 with score 0.60
				Expect(candidates[1].SourceID).To(Equal("AC-1"))
				Expect(candidates[1].TargetID).To(Equal("AC-3"))
				Expect(candidates[1].Score).To(BeNumerically("~", 0.60, 0.01))
				Expect(candidates[1].Weight).To(Equal(1.0))
				Expect(candidates[1].GeneratorID).To(Equal("semantic"))
			})
		})

		Context("when filtering by minimum similarity", func() {
			It("excludes targets below the threshold", func() {
				matrix := &SimilarityMatrix{
					IDs: []string{"AC-1", "AC-2", "AC-3"},
					Values: [][]float32{
						{100, 80, 40}, // AC-3 below threshold
						{80, 100, 70},
						{40, 70, 100},
					},
				}

				sources := map[string]*candidate.ControlData{
					"AC-1": {ControlID: "AC-1", Text: "Source control 1"},
				}

				targets := map[string]*candidate.ControlData{
					"AC-2": {ControlID: "AC-2", Text: "Target control 2"},
					"AC-3": {ControlID: "AC-3", Text: "Target control 3"},
				}

				req := candidate.GenerateRequest{
					TenantID:        "test-tenant",
					JobID:           "test-job",
					SourceControls:  sources,
					TargetControls:  targets,
					EmbeddingMatrix: matrix,
					Parameters: map[string]interface{}{
						"top_k":          10,
						"min_similarity": 50.0, // Filter out AC-3 (40)
						"weight":         0.8,
					},
				}

				candidates, err := gen.Generate(context.Background(), req)
				Expect(err).NotTo(HaveOccurred())

				// Should only return AC-2 (80 >= 50)
				Expect(candidates).To(HaveLen(1))
				Expect(candidates[0].TargetID).To(Equal("AC-2"))
				Expect(candidates[0].Score).To(BeNumerically("~", 0.80, 0.01))
				Expect(candidates[0].Weight).To(Equal(0.8))
			})
		})

		Context("when no embedding matrix is provided", func() {
			It("returns an empty slice", func() {
				req := candidate.GenerateRequest{
					TenantID:       "test-tenant",
					JobID:          "test-job",
					SourceControls: map[string]*candidate.ControlData{},
					TargetControls: map[string]*candidate.ControlData{},
					// No EmbeddingMatrix provided
					Parameters: map[string]interface{}{
						"top_k":          10,
						"min_similarity": 50.0,
						"weight":         1.0,
					},
				}

				candidates, err := gen.Generate(context.Background(), req)
				Expect(err).NotTo(HaveOccurred())
				Expect(candidates).To(BeEmpty(), "Should return empty slice when no embedding matrix")
			})
		})

		Context("when multiple sources each select top-1 target", func() {
			It("returns one candidate per source with the highest similarity", func() {
				// 4x4 matrix: 2 sources (AC-1, AC-2) and 2 targets (AC-3, AC-4)
				matrix := &SimilarityMatrix{
					IDs: []string{"AC-1", "AC-2", "AC-3", "AC-4"},
					Values: [][]float32{
						{100, 50, 90, 70},
						{50, 100, 60, 85},
						{90, 60, 100, 55},
						{70, 85, 55, 100},
					},
				}

				sources := map[string]*candidate.ControlData{
					"AC-1": {ControlID: "AC-1", Text: "Source 1"},
					"AC-2": {ControlID: "AC-2", Text: "Source 2"},
				}

				targets := map[string]*candidate.ControlData{
					"AC-3": {ControlID: "AC-3", Text: "Target 3"},
					"AC-4": {ControlID: "AC-4", Text: "Target 4"},
				}

				req := candidate.GenerateRequest{
					TenantID:        "test-tenant",
					JobID:           "test-job",
					SourceControls:  sources,
					TargetControls:  targets,
					EmbeddingMatrix: matrix,
					Parameters: map[string]interface{}{
						"top_k":          1, // Only top-1 per source
						"min_similarity": 60.0,
						"weight":         1.0,
					},
				}

				candidates, err := gen.Generate(context.Background(), req)
				Expect(err).NotTo(HaveOccurred())

				// Should have 2 candidates: top-1 for AC-1 and top-1 for AC-2
				// AC-1 -> AC-3 (90)
				// AC-2 -> AC-4 (85)
				Expect(candidates).To(HaveLen(2))

				// Find each candidate
				var ac1Candidate, ac2Candidate *candidate.Candidate
				for i := range candidates {
					switch candidates[i].SourceID {
					case "AC-1":
						ac1Candidate = &candidates[i]
					case "AC-2":
						ac2Candidate = &candidates[i]
					}
				}

				Expect(ac1Candidate).NotTo(BeNil())
				Expect(ac1Candidate.TargetID).To(Equal("AC-3"))
				Expect(ac1Candidate.Score).To(BeNumerically("~", 0.90, 0.01))

				Expect(ac2Candidate).NotTo(BeNil())
				Expect(ac2Candidate.TargetID).To(Equal("AC-4"))
				Expect(ac2Candidate.Score).To(BeNumerically("~", 0.85, 0.01))
			})
		})

		Context("when telemetry is configured", func() {
			It("generates candidates with a noop tracer", func() {
				tracer := noop.NewTracerProvider().Tracer("test")
				genWithTelemetry := builtin.NewSemanticGenerator(builtin.WithSemanticTelemetry(tracer))

				matrix := &SimilarityMatrix{
					IDs: []string{"AC-1", "AC-2"},
					Values: [][]float32{
						{100, 80},
						{80, 100},
					},
				}

				sources := map[string]*candidate.ControlData{
					"AC-1": {ControlID: "AC-1", Text: "Source"},
				}

				targets := map[string]*candidate.ControlData{
					"AC-2": {ControlID: "AC-2", Text: "Target"},
				}

				req := candidate.GenerateRequest{
					TenantID:        "test-tenant",
					SourceControls:  sources,
					TargetControls:  targets,
					EmbeddingMatrix: matrix,
					Parameters: map[string]interface{}{
						"top_k": 1,
					},
				}

				candidates, err := genWithTelemetry.Generate(context.Background(), req)
				Expect(err).NotTo(HaveOccurred())
				Expect(candidates).NotTo(BeEmpty())
				Expect(candidates).To(HaveLen(1))
			})
		})
	})
})
