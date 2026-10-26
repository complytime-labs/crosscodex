package pipeline

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
)

// relProps returns the Properties the relationship analyzer persists for a
// pair with the given contribution type and unanimity.
func relProps(contributionType, unanimous string) map[string]string {
	return map[string]string{
		results.SemanticMatchContributionTypeKey: contributionType,
		results.SemanticMatchUnanimousKey:        unanimous,
		"valid_vote_count":                       "3",
	}
}

var _ = Describe("buildSynthesisInputs", func() {
	It("builds a requires row", func() {
		requires := []results.RequiresResult{{SourceID: "AC-1", TargetID: "AC-2", Confidence: 0.9, Unanimous: true}}
		inputs, _, err := buildSynthesisInputs(nil, requires, nil, nil)
		Expect(err).NotTo(HaveOccurred())

		Expect(inputs).To(HaveLen(1))
		got := inputs[0]
		Expect(got.ConsensusRelationship).To(Equal("requires"))
		Expect(got.Analyzer).To(Equal(results.RequiresAnalyzer))
		Expect(got.ContributionType).To(BeEmpty(), "requires is not a CONTRIBUTES_TO sub-classification")
		Expect(got.ConfidenceFraction).To(Equal(0.9))
		Expect(got.Unanimous).To(BeTrue())
	})

	It("builds a relationship row from the analyzer's persisted properties", func() {
		rel := []results.SemanticMatchResult{{SourceID: "AC-1", TargetID: "AC-3", RelationshipType: "CONTRIBUTES_TO", Confidence: 0.7,
			Properties: relProps("INTEGRAL_TO", "true")}}
		inputs, _, err := buildSynthesisInputs(nil, nil, rel, nil)
		Expect(err).NotTo(HaveOccurred())

		Expect(inputs).To(HaveLen(1))
		got := inputs[0]
		Expect(got.ConsensusRelationship).To(Equal("CONTRIBUTES_TO"))
		Expect(got.Analyzer).To(Equal(results.RelationshipAnalyzer))
		Expect(got.ContributionType).To(Equal("INTEGRAL_TO"))
		Expect(got.ConfidenceFraction).To(Equal(0.7))
		Expect(got.Unanimous).To(BeTrue())
	})

	DescribeTable("carries the relationship analyzer's contribution type and unanimity",
		func(contributionType, unanimous string, wantUnanimous bool) {
			rel := []results.SemanticMatchResult{{SourceID: "AC-1", TargetID: "AC-3", RelationshipType: "SUPPORTS", Confidence: 0.7,
				Properties: relProps(contributionType, unanimous)}}
			inputs, _, err := buildSynthesisInputs(nil, nil, rel, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(inputs).To(HaveLen(1))
			Expect(inputs[0].ContributionType).To(Equal(contributionType))
			Expect(inputs[0].Unanimous).To(Equal(wantUnanimous))
		},
		Entry("INTEGRAL_TO, unanimous", "INTEGRAL_TO", "true", true),
		Entry("EXAMPLE_OF, split vote", "EXAMPLE_OF", "false", false),
		Entry("no sub-classification (not CONTRIBUTES_TO), split vote", "", "false", false),
	)

	DescribeTable("rejects corrupt relationship properties with an actionable error and builds nothing",
		func(props map[string]string, wantInMessage string) {
			requires := []results.RequiresResult{{SourceID: "AC-1", TargetID: "AC-2", Confidence: 0.9}}
			rel := []results.SemanticMatchResult{{SourceID: "AC-1", TargetID: "AC-3", RelationshipType: "SUPPORTS", Confidence: 0.7,
				Properties: props}}
			inputs, classifications, err := buildSynthesisInputs(nil, requires, rel, nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("AC-1--AC-3"))
			Expect(err.Error()).To(ContainSubstring(wantInMessage))
			Expect(err.Error()).To(ContainSubstring("create a new job"), "the message says what to do instead")
			Expect(inputs).To(BeNil(), "no partial inputs reach synthesis")
			Expect(classifications).To(BeNil())
		},
		Entry("missing properties", nil, `"contribution_type"`),
		Entry("missing contribution type", map[string]string{results.SemanticMatchUnanimousKey: "true"}, `"contribution_type"`),
		Entry("unknown contribution type", relProps("NONE", "true"), `"NONE"`),
		Entry("lower-case contribution type", relProps("integral_to", "true"), `"integral_to"`),
		Entry("missing unanimity", map[string]string{results.SemanticMatchContributionTypeKey: "INTEGRAL_TO"}, `"unanimous"`),
		Entry("non-boolean unanimity", relProps("INTEGRAL_TO", "yes"), `"yes"`),
	)

	It("produces two rows for the same pair when both analyzers report it", func() {
		requires := []results.RequiresResult{{SourceID: "AC-1", TargetID: "AC-2", Confidence: 0.9}}
		rel := []results.SemanticMatchResult{{SourceID: "AC-1", TargetID: "AC-2", RelationshipType: "SUPPORTS", Confidence: 0.7,
			Properties: relProps("", "true")}}
		inputs, _, err := buildSynthesisInputs(nil, requires, rel, nil)
		Expect(err).NotTo(HaveOccurred())

		Expect(inputs).To(HaveLen(2), "same pair, two analyzers")
	})

	It("computes similarity count, mean score, and median across models", func() {
		similarity := map[string]similarityMatrix{
			"model-a": {IDs: []string{"AC-1", "AC-2"}, Values: [][]float32{{0, 80}, {80, 0}}},
			"model-b": {IDs: []string{"AC-1", "AC-2"}, Values: [][]float32{{0, 90}, {90, 0}}},
		}
		requires := []results.RequiresResult{{SourceID: "AC-1", TargetID: "AC-2", Confidence: 0.9}}
		inputs, _, err := buildSynthesisInputs(nil, requires, nil, similarity)
		Expect(err).NotTo(HaveOccurred())

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
		inputs, _, err := buildSynthesisInputs(nil, requires, nil, similarity)
		Expect(err).NotTo(HaveOccurred())

		got := inputs[0]
		Expect(got.SimilarityCount).To(Equal(3))
		Expect(got.SimilarityMedian).To(Equal(85.0), "middle value of [80, 85, 90]")
	})

	It("builds the classifications map keyed by control ID", func() {
		classify := []results.ClassifyResult{{ControlID: "AC-1", Type: "Technical", Level: "Operational"}}
		_, classifications, err := buildSynthesisInputs(classify, nil, nil, nil)
		Expect(err).NotTo(HaveOccurred())

		got, ok := classifications["AC-1"]
		Expect(ok).To(BeTrue())
		Expect(got.Type).To(Equal("Technical"))
		Expect(got.Level).To(Equal("Operational"))
	})
})
