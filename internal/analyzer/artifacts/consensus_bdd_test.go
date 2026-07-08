package artifacts_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer"
	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
)

var _ = Describe("ComputeConsensus", func() {
	threshold := analyzer.DefaultFuzzyThreshold

	// Python parity: test_empty_votes_returns_empty
	It("returns empty for zero totalVoters", func() {
		result := artifacts.ComputeConsensus(nil, 0, threshold)
		Expect(result).To(BeEmpty())
	})

	It("returns empty for empty votes map", func() {
		result := artifacts.ComputeConsensus(map[string]*artifacts.Vote{}, 2, threshold)
		Expect(result).To(BeEmpty())
	})

	// Python parity: test_single_voter_confidence_1
	It("returns confidence 1.0 for single voter", func() {
		votes := map[string]*artifacts.Vote{
			"model1": {
				VoteKey:     "model1",
				ParseStatus: artifacts.ParseOK,
				Artifacts: []artifacts.ExtractedArtifact{
					{Name: "Policy", Type: artifacts.ArtifactPolicy},
				},
			},
		}
		result := artifacts.ComputeConsensus(votes, 1, threshold)
		Expect(result).To(HaveLen(1))
		Expect(result[0].Confidence).To(Equal(1.0))
		Expect(result[0].Unanimous).To(BeTrue())
	})

	// Python parity: test_two_voters_same_artifact_confidence_1
	It("fuzzy-matches same artifact across voters", func() {
		votes := map[string]*artifacts.Vote{
			"model1": {
				VoteKey: "model1", ParseStatus: artifacts.ParseOK,
				Artifacts: []artifacts.ExtractedArtifact{
					{Name: "Policy Doc", Type: artifacts.ArtifactPolicy},
				},
			},
			"model2": {
				VoteKey: "model2", ParseStatus: artifacts.ParseOK,
				Artifacts: []artifacts.ExtractedArtifact{
					{Name: "The Policy Doc", Type: artifacts.ArtifactPolicy},
				},
			},
		}
		result := artifacts.ComputeConsensus(votes, 2, threshold)
		Expect(result).To(HaveLen(1))
		Expect(result[0].Confidence).To(Equal(1.0))
		Expect(result[0].VoteCount).To(Equal(2))
	})

	// Python parity: test_two_voters_different_artifacts
	It("keeps distinct artifacts separate", func() {
		votes := map[string]*artifacts.Vote{
			"model1": {
				VoteKey: "model1", ParseStatus: artifacts.ParseOK,
				Artifacts: []artifacts.ExtractedArtifact{
					{Name: "Access Policy", Type: artifacts.ArtifactPolicy},
				},
			},
			"model2": {
				VoteKey: "model2", ParseStatus: artifacts.ParseOK,
				Artifacts: []artifacts.ExtractedArtifact{
					{Name: "Audit Report", Type: artifacts.ArtifactReport},
				},
			},
		}
		result := artifacts.ComputeConsensus(votes, 2, threshold)
		Expect(result).To(HaveLen(2))
		for _, art := range result {
			Expect(art.Confidence).To(Equal(0.5))
		}
	})

	// Python parity: test_all_empty_artifact_lists
	It("returns empty when all votes have empty artifact lists", func() {
		votes := map[string]*artifacts.Vote{
			"model1": {VoteKey: "model1", ParseStatus: artifacts.ParseOK, Artifacts: nil},
			"model2": {VoteKey: "model2", ParseStatus: artifacts.ParseOK, Artifacts: nil},
		}
		result := artifacts.ComputeConsensus(votes, 2, threshold)
		Expect(result).To(BeEmpty())
	})

	// Python parity: test_majority_type_wins
	It("uses majority vote for type", func() {
		votes := map[string]*artifacts.Vote{
			"m1": {VoteKey: "m1", ParseStatus: artifacts.ParseOK, Artifacts: []artifacts.ExtractedArtifact{
				{Name: "Doc", Type: artifacts.ArtifactPolicy},
			}},
			"m2": {VoteKey: "m2", ParseStatus: artifacts.ParseOK, Artifacts: []artifacts.ExtractedArtifact{
				{Name: "Doc", Type: artifacts.ArtifactProcedure},
			}},
			"m3": {VoteKey: "m3", ParseStatus: artifacts.ParseOK, Artifacts: []artifacts.ExtractedArtifact{
				{Name: "The Doc", Type: artifacts.ArtifactPolicy},
			}},
		}
		result := artifacts.ComputeConsensus(votes, 3, threshold)
		Expect(result[0].Type).To(Equal(artifacts.ArtifactPolicy))
	})

	// Python parity: test_description_uses_longest
	It("uses longest description", func() {
		votes := map[string]*artifacts.Vote{
			"m1": {VoteKey: "m1", ParseStatus: artifacts.ParseOK, Artifacts: []artifacts.ExtractedArtifact{
				{Name: "Doc", Type: artifacts.ArtifactPolicy, Description: "Short"},
			}},
			"m2": {VoteKey: "m2", ParseStatus: artifacts.ParseOK, Artifacts: []artifacts.ExtractedArtifact{
				{Name: "Doc", Type: artifacts.ArtifactPolicy, Description: "This is a longer description"},
			}},
		}
		result := artifacts.ComputeConsensus(votes, 2, threshold)
		Expect(result[0].Description).To(Equal("This is a longer description"))
	})

	It("skips votes with ParseFail status", func() {
		votes := map[string]*artifacts.Vote{
			"m1": {VoteKey: "m1", ParseStatus: artifacts.ParseOK, Artifacts: []artifacts.ExtractedArtifact{
				{Name: "Policy", Type: artifacts.ArtifactPolicy},
			}},
			"m2": {VoteKey: "m2", ParseStatus: artifacts.ParseFail},
		}
		result := artifacts.ComputeConsensus(votes, 2, threshold)
		Expect(result).To(HaveLen(1))
		Expect(result[0].Confidence).To(Equal(0.5))
		Expect(result[0].VoteCount).To(Equal(1))
	})

	It("sorts by confidence descending", func() {
		votes := map[string]*artifacts.Vote{
			"m1": {VoteKey: "m1", ParseStatus: artifacts.ParseOK, Artifacts: []artifacts.ExtractedArtifact{
				{Name: "Common Policy", Type: artifacts.ArtifactPolicy},
				{Name: "Rare Report", Type: artifacts.ArtifactReport},
			}},
			"m2": {VoteKey: "m2", ParseStatus: artifacts.ParseOK, Artifacts: []artifacts.ExtractedArtifact{
				{Name: "Common Policy", Type: artifacts.ArtifactPolicy},
			}},
		}
		result := artifacts.ComputeConsensus(votes, 2, threshold)
		Expect(result).To(HaveLen(2))
		Expect(result[0].Confidence).To(BeNumerically(">", result[1].Confidence))
	})

	It("merges Properties with first-writer-wins", func() {
		votes := map[string]*artifacts.Vote{
			"m1": {VoteKey: "m1", ParseStatus: artifacts.ParseOK, Artifacts: []artifacts.ExtractedArtifact{
				{Name: "Policy", Type: artifacts.ArtifactPolicy, Properties: map[string]string{"CITATION": "NIST 800-53"}},
			}},
			"m2": {VoteKey: "m2", ParseStatus: artifacts.ParseOK, Artifacts: []artifacts.ExtractedArtifact{
				{Name: "Policy", Type: artifacts.ArtifactPolicy, Properties: map[string]string{"CITATION": "SP 800-53", "EXTRA": "val"}},
			}},
		}
		result := artifacts.ComputeConsensus(votes, 2, threshold)
		Expect(result[0].Properties).To(HaveKeyWithValue("CITATION", "NIST 800-53"))
		Expect(result[0].Properties).To(HaveKeyWithValue("EXTRA", "val"))
	})
})
