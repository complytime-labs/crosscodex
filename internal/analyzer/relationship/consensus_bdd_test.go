package relationship_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/relationship"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

// defaultConfidenceWeights is the compiled default
// analysis.relationship.confidence_weights.
var defaultConfidenceWeights = config.RelationshipConfidenceWeights{High: 1.0, Medium: 0.67, Low: 0.33}

var _ = Describe("ComputeConsensus", func() {
	weights := defaultConfidenceWeights

	// Helper: build a valid vote for a given relationship type.
	makeVote := func(rel relationship.RelationshipType, contrib relationship.ContributionType, conf relationship.ConfidenceLevel) *relationship.Vote {
		return &relationship.Vote{
			Relationship:     rel,
			ContributionType: contrib,
			Confidence:       conf,
			ParseStatus:      relationship.ParseOK,
		}
	}

	// Helper: build a parse-error vote.
	makeErrorVote := func(status relationship.ParseStatus) *relationship.Vote {
		return &relationship.Vote{
			ParseStatus: status,
		}
	}

	// Python parity: test_compute_consensus_unanimous
	It("returns winner with confidence 1.0 when unanimous", func() {
		votes := map[string]*relationship.Vote{
			"qwen3":   makeVote(relationship.RelSupersetOf, relationship.ContribNone, relationship.ConfidenceHigh),
			"mistral": makeVote(relationship.RelSupersetOf, relationship.ContribNone, relationship.ConfidenceHigh),
		}
		c := relationship.ComputeConsensus(votes, weights)
		Expect(c.Relationship).To(Equal(relationship.RelSupersetOf))
		Expect(c.Unanimous).To(BeTrue())
		Expect(c.AgreementFraction).To(Equal(1.0))
		Expect(c.ConfidenceFraction).To(Equal(1.0))
		Expect(c.ValidVoteCount).To(Equal(2))
	})

	// Python parity: test_compute_consensus_plurality
	It("returns majority winner with correct confidence fraction", func() {
		votes := map[string]*relationship.Vote{
			"qwen3":       makeVote(relationship.RelSupersetOf, relationship.ContribNone, relationship.ConfidenceHigh),
			"llama3.1:8b": makeVote(relationship.RelSupersetOf, relationship.ContribNone, relationship.ConfidenceHigh),
			"mistral":     makeVote(relationship.RelContributesTo, relationship.ContribNone, relationship.ConfidenceMedium),
		}
		c := relationship.ComputeConsensus(votes, weights)
		Expect(c.Relationship).To(Equal(relationship.RelSupersetOf))
		Expect(c.Unanimous).To(BeFalse())
		Expect(c.AgreementFraction).To(Equal(0.667))
		Expect(c.ConfidenceFraction).To(Equal(0.667))
		Expect(c.ValidVoteCount).To(Equal(3))
	})

	// Python parity: test_compute_consensus_tiebreak
	It("breaks ties by priority (EQUIVALENT > SUPERSET_OF)", func() {
		votes := map[string]*relationship.Vote{
			"qwen3":   makeVote(relationship.RelEquivalent, relationship.ContribNone, relationship.ConfidenceHigh),
			"mistral": makeVote(relationship.RelSupersetOf, relationship.ContribNone, relationship.ConfidenceHigh),
		}
		c := relationship.ComputeConsensus(votes, weights)
		Expect(c.Relationship).To(Equal(relationship.RelEquivalent))
		Expect(c.AgreementFraction).To(Equal(0.5))
		Expect(c.ConfidenceFraction).To(Equal(0.5))
	})

	// Python parity: test_compute_consensus_excludes_parse_errors
	It("excludes PARSE_ERROR and PARSE_FAIL from consensus", func() {
		votes := map[string]*relationship.Vote{
			"qwen3":       makeVote(relationship.RelSupersetOf, relationship.ContribNone, relationship.ConfidenceHigh),
			"llama3.1:8b": makeErrorVote(relationship.ParseError),
			"mistral":     makeErrorVote(relationship.ParseFail),
		}
		c := relationship.ComputeConsensus(votes, weights)
		Expect(c.Relationship).To(Equal(relationship.RelSupersetOf))
		Expect(c.ValidVoteCount).To(Equal(1))
		Expect(c.ConfidenceFraction).To(Equal(1.0))
	})

	// Python parity: test_compute_consensus_all_errors
	It("returns NO_RELATIONSHIP with confidence 0.0 when all votes are errors", func() {
		votes := map[string]*relationship.Vote{
			"qwen3":   makeErrorVote(relationship.ParseError),
			"mistral": makeErrorVote(relationship.ParseFail),
		}
		c := relationship.ComputeConsensus(votes, weights)
		Expect(c.Relationship).To(Equal(relationship.RelNoRelationship))
		Expect(c.ContributionType).To(Equal(relationship.ContribNone))
		Expect(c.AgreementFraction).To(Equal(0.0))
		Expect(c.ConfidenceFraction).To(Equal(0.0))
		Expect(c.ValidVoteCount).To(Equal(0))
		Expect(c.Unanimous).To(BeFalse())
	})

	// Python parity: test_compute_consensus_contribution_type_majority
	It("computes secondary contribution type consensus when winner is CONTRIBUTES_TO", func() {
		votes := map[string]*relationship.Vote{
			"qwen3":       makeVote(relationship.RelContributesTo, relationship.ContribIntegralTo, relationship.ConfidenceHigh),
			"llama3.1:8b": makeVote(relationship.RelContributesTo, relationship.ContribIntegralTo, relationship.ConfidenceHigh),
			"mistral":     makeVote(relationship.RelContributesTo, relationship.ContribExampleOf, relationship.ConfidenceMedium),
		}
		c := relationship.ComputeConsensus(votes, weights)
		Expect(c.Relationship).To(Equal(relationship.RelContributesTo))
		Expect(c.ContributionType).To(Equal(relationship.ContribIntegralTo))
		Expect(c.AgreementFraction).To(Equal(1.0))
		Expect(c.ConfidenceFraction).To(Equal(0.89)) // (1.0 + 1.0 + 0.67) / 3
		Expect(c.Unanimous).To(BeTrue())
	})

	// Python parity: test_compute_consensus_contribution_type_non_contributes
	It("returns empty contribution type when winner is not CONTRIBUTES_TO", func() {
		votes := map[string]*relationship.Vote{
			"qwen3":   makeVote(relationship.RelEquivalent, relationship.ContribNone, relationship.ConfidenceHigh),
			"mistral": makeVote(relationship.RelEquivalent, relationship.ContribNone, relationship.ConfidenceHigh),
		}
		c := relationship.ComputeConsensus(votes, weights)
		Expect(c.Relationship).To(Equal(relationship.RelEquivalent))
		Expect(c.ContributionType).To(Equal(relationship.ContribNone))
	})

	// Python parity: test_compute_consensus_contribution_type_mixed_relationship
	It("only considers CONTRIBUTES_TO voters for contribution type consensus", func() {
		votes := map[string]*relationship.Vote{
			"qwen3":       makeVote(relationship.RelContributesTo, relationship.ContribExampleOf, relationship.ConfidenceHigh),
			"llama3.1:8b": makeVote(relationship.RelContributesTo, relationship.ContribExampleOf, relationship.ConfidenceHigh),
			"mistral":     makeVote(relationship.RelSupersetOf, relationship.ContribNone, relationship.ConfidenceHigh),
		}
		c := relationship.ComputeConsensus(votes, weights)
		Expect(c.Relationship).To(Equal(relationship.RelContributesTo))
		Expect(c.ContributionType).To(Equal(relationship.ContribExampleOf))
	})

	It("returns NO_RELATIONSHIP for empty votes map", func() {
		c := relationship.ComputeConsensus(map[string]*relationship.Vote{}, weights)
		Expect(c.Relationship).To(Equal(relationship.RelNoRelationship))
		Expect(c.AgreementFraction).To(Equal(0.0))
		Expect(c.ConfidenceFraction).To(Equal(0.0))
		Expect(c.ValidVoteCount).To(Equal(0))
		Expect(c.Unanimous).To(BeFalse())
	})

	It("handles single vote as trivially unanimous", func() {
		votes := map[string]*relationship.Vote{
			"qwen3": makeVote(relationship.RelSubsetOf, relationship.ContribNone, relationship.ConfidenceHigh),
		}
		c := relationship.ComputeConsensus(votes, weights)
		Expect(c.Relationship).To(Equal(relationship.RelSubsetOf))
		Expect(c.Unanimous).To(BeTrue())
		Expect(c.ConfidenceFraction).To(Equal(1.0))
		Expect(c.ValidVoteCount).To(Equal(1))
	})

	// Documented divergence: INTEGRAL_TO wins ties (Python is non-deterministic)
	It("breaks contribution type tie deterministically favoring INTEGRAL_TO", func() {
		votes := map[string]*relationship.Vote{
			"qwen3":   makeVote(relationship.RelContributesTo, relationship.ContribIntegralTo, relationship.ConfidenceHigh),
			"mistral": makeVote(relationship.RelContributesTo, relationship.ContribExampleOf, relationship.ConfidenceHigh),
		}
		c := relationship.ComputeConsensus(votes, weights)
		Expect(c.Relationship).To(Equal(relationship.RelContributesTo))
		Expect(c.ContributionType).To(Equal(relationship.ContribIntegralTo))
	})

	It("includes all votes in AllVotes including parse errors", func() {
		votes := map[string]*relationship.Vote{
			"qwen3":   makeVote(relationship.RelEquivalent, relationship.ContribNone, relationship.ConfidenceHigh),
			"mistral": makeErrorVote(relationship.ParseError),
		}
		c := relationship.ComputeConsensus(votes, weights)
		Expect(c.AllVotes).To(HaveLen(2))
		Expect(c.AllVotes).To(ContainElements("EQUIVALENT", "PARSE_ERROR"))
	})

	DescribeTable("scales agreement by the mean confidence weight of the winning votes",
		func(votes map[string]*relationship.Vote, w config.RelationshipConfidenceWeights,
			wantRel relationship.RelationshipType, wantAgreement, wantConfidence float64) {
			c := relationship.ComputeConsensus(votes, w)
			Expect(c.Relationship).To(Equal(wantRel))
			Expect(c.AgreementFraction).To(Equal(wantAgreement))
			Expect(c.ConfidenceFraction).To(Equal(wantConfidence))
		},
		Entry("one MEDIUM vote scores the medium weight", map[string]*relationship.Vote{
			"m__s0": makeVote(relationship.RelEquivalent, relationship.ContribNone, relationship.ConfidenceMedium),
		}, weights, relationship.RelEquivalent, 1.0, 0.67),
		Entry("one LOW vote scores the low weight", map[string]*relationship.Vote{
			"m__s0": makeVote(relationship.RelEquivalent, relationship.ContribNone, relationship.ConfidenceLow),
		}, weights, relationship.RelEquivalent, 1.0, 0.33),
		Entry("a vote with no reported confidence scores the low weight", map[string]*relationship.Vote{
			"m__s0": makeVote(relationship.RelEquivalent, relationship.ContribNone, relationship.ConfidenceUnknown),
		}, weights, relationship.RelEquivalent, 1.0, 0.33),
		Entry("two of three agreeing, HIGH and LOW, score 2/3 x 0.665", map[string]*relationship.Vote{
			"m__s0": makeVote(relationship.RelSubsetOf, relationship.ContribNone, relationship.ConfidenceHigh),
			"m__s1": makeVote(relationship.RelSubsetOf, relationship.ContribNone, relationship.ConfidenceLow),
			"m__s2": makeVote(relationship.RelPartial, relationship.ContribNone, relationship.ConfidenceHigh),
		}, weights, relationship.RelSubsetOf, 0.667, 0.443),
		Entry("a losing vote's confidence does not count", map[string]*relationship.Vote{
			"m__s0": makeVote(relationship.RelSubsetOf, relationship.ContribNone, relationship.ConfidenceHigh),
			"m__s1": makeVote(relationship.RelSubsetOf, relationship.ContribNone, relationship.ConfidenceHigh),
			"m__s2": makeVote(relationship.RelPartial, relationship.ContribNone, relationship.ConfidenceLow),
		}, weights, relationship.RelSubsetOf, 0.667, 0.667),
		Entry("the winner is still the plurality, not the most confident type", map[string]*relationship.Vote{
			"m__s0": makeVote(relationship.RelEquivalent, relationship.ContribNone, relationship.ConfidenceLow),
			"m__s1": makeVote(relationship.RelEquivalent, relationship.ContribNone, relationship.ConfidenceLow),
			"m__s2": makeVote(relationship.RelSupersetOf, relationship.ContribNone, relationship.ConfidenceHigh),
		}, weights, relationship.RelEquivalent, 0.667, 0.22),
		Entry("equal weights of 1 reduce confidence to agreement", map[string]*relationship.Vote{
			"m__s0": makeVote(relationship.RelEquivalent, relationship.ContribNone, relationship.ConfidenceLow),
		}, config.RelationshipConfidenceWeights{High: 1, Medium: 1, Low: 1}, relationship.RelEquivalent, 1.0, 1.0),
	)
})
