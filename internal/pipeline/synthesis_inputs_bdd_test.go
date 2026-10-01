package pipeline

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
)

var _ = Describe("buildSynthesisInputs", func() {
	It("builds a requires row", func() {
		requires := []results.RequiresResult{{SourceID: "AC-1", TargetID: "AC-2", Confidence: 0.9, Unanimous: true}}
		inputs, _ := buildSynthesisInputs(nil, requires, nil, nil)

		Expect(inputs).To(HaveLen(1))
		got := inputs[0]
		Expect(got.ConsensusRelationship).To(Equal("requires"))
		Expect(got.ContributionType).To(Equal("requires"))
		Expect(got.ConfidenceFraction).To(Equal(0.9))
		Expect(got.Unanimous).To(BeTrue())
	})

	It("builds a relationship row", func() {
		rel := []results.SemanticMatchResult{{SourceID: "AC-1", TargetID: "AC-3", RelationshipType: "supports", Confidence: 0.7}}
		inputs, _ := buildSynthesisInputs(nil, nil, rel, nil)

		Expect(inputs).To(HaveLen(1))
		got := inputs[0]
		Expect(got.ConsensusRelationship).To(Equal("supports"))
		Expect(got.ContributionType).To(Equal("relationship"))
		Expect(got.ConfidenceFraction).To(Equal(0.7))
		Expect(got.Unanimous).To(BeTrue(), "relationship rows are Unanimous=true per spec table")
	})

	It("produces two rows for the same pair when both contribution types are present", func() {
		requires := []results.RequiresResult{{SourceID: "AC-1", TargetID: "AC-2", Confidence: 0.9}}
		rel := []results.SemanticMatchResult{{SourceID: "AC-1", TargetID: "AC-2", RelationshipType: "supports", Confidence: 0.7}}
		inputs, _ := buildSynthesisInputs(nil, requires, rel, nil)

		Expect(inputs).To(HaveLen(2), "same pair, two contribution types")
	})

	It("computes similarity count, mean score, and median across models", func() {
		similarity := map[string]similarityMatrix{
			"model-a": {IDs: []string{"AC-1", "AC-2"}, Values: [][]float32{{0, 80}, {80, 0}}},
			"model-b": {IDs: []string{"AC-1", "AC-2"}, Values: [][]float32{{0, 90}, {90, 0}}},
		}
		requires := []results.RequiresResult{{SourceID: "AC-1", TargetID: "AC-2", Confidence: 0.9}}
		inputs, _ := buildSynthesisInputs(nil, requires, nil, similarity)

		got := inputs[0]
		Expect(got.SimilarityCount).To(Equal(2))
		Expect(got.SimilarityScore).To(Equal(85.0), "mean(80, 90)")
		Expect(got.SimilarityMedian).To(Equal(85.0), "average of the two middle values, 80 and 90")
	})

	It("computes the similarity median for an odd count of models", func() {
		similarity := map[string]similarityMatrix{
			"model-a": {IDs: []string{"AC-1", "AC-2"}, Values: [][]float32{{0, 80}, {80, 0}}},
			"model-b": {IDs: []string{"AC-1", "AC-2"}, Values: [][]float32{{0, 85}, {85, 0}}},
			"model-c": {IDs: []string{"AC-1", "AC-2"}, Values: [][]float32{{0, 90}, {90, 0}}},
		}
		requires := []results.RequiresResult{{SourceID: "AC-1", TargetID: "AC-2", Confidence: 0.9}}
		inputs, _ := buildSynthesisInputs(nil, requires, nil, similarity)

		got := inputs[0]
		Expect(got.SimilarityCount).To(Equal(3))
		Expect(got.SimilarityMedian).To(Equal(85.0), "middle value of [80, 85, 90]")
	})

	It("builds the classifications map keyed by control ID", func() {
		classify := []results.ClassifyResult{{ControlID: "AC-1", Type: "Technical", Level: "Operational"}}
		_, classifications := buildSynthesisInputs(classify, nil, nil, nil)

		got, ok := classifications["AC-1"]
		Expect(ok).To(BeTrue())
		Expect(got.Type).To(Equal("Technical"))
		Expect(got.Level).To(Equal("Operational"))
	})
})
