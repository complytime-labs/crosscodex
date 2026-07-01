package relationship_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/relationship"
)

var _ = Describe("ComputeConsensus", func() {
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
		c := relationship.ComputeConsensus(votes)
		Expect(c.Relationship).To(Equal(relationship.RelSupersetOf))
		Expect(c.Unanimous).To(BeTrue())
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
		c := relationship.ComputeConsensus(votes)
		Expect(c.Relationship).To(Equal(relationship.RelSupersetOf))
		Expect(c.Unanimous).To(BeFalse())
		Expect(c.ConfidenceFraction).To(BeNumerically("~", 0.667, 0.001))
		Expect(c.ValidVoteCount).To(Equal(3))
	})

	// Python parity: test_compute_consensus_tiebreak
	It("breaks ties by priority (EQUIVALENT > SUPERSET_OF)", func() {
		votes := map[string]*relationship.Vote{
			"qwen3":   makeVote(relationship.RelEquivalent, relationship.ContribNone, relationship.ConfidenceHigh),
			"mistral": makeVote(relationship.RelSupersetOf, relationship.ContribNone, relationship.ConfidenceHigh),
		}
		c := relationship.ComputeConsensus(votes)
		Expect(c.Relationship).To(Equal(relationship.RelEquivalent))
		Expect(c.ConfidenceFraction).To(Equal(0.5))
	})

	// Python parity: test_compute_consensus_excludes_parse_errors
	It("excludes PARSE_ERROR and PARSE_FAIL from consensus", func() {
		votes := map[string]*relationship.Vote{
			"qwen3":       makeVote(relationship.RelSupersetOf, relationship.ContribNone, relationship.ConfidenceHigh),
			"llama3.1:8b": makeErrorVote(relationship.ParseError),
			"mistral":     makeErrorVote(relationship.ParseFail),
		}
		c := relationship.ComputeConsensus(votes)
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
		c := relationship.ComputeConsensus(votes)
		Expect(c.Relationship).To(Equal(relationship.RelNoRelationship))
		Expect(c.ContributionType).To(Equal(relationship.ContribNone))
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
		c := relationship.ComputeConsensus(votes)
		Expect(c.Relationship).To(Equal(relationship.RelContributesTo))
		Expect(c.ContributionType).To(Equal(relationship.ContribIntegralTo))
		Expect(c.ConfidenceFraction).To(Equal(1.0))
		Expect(c.Unanimous).To(BeTrue())
	})

	// Python parity: test_compute_consensus_contribution_type_non_contributes
	It("returns empty contribution type when winner is not CONTRIBUTES_TO", func() {
		votes := map[string]*relationship.Vote{
			"qwen3":   makeVote(relationship.RelEquivalent, relationship.ContribNone, relationship.ConfidenceHigh),
			"mistral": makeVote(relationship.RelEquivalent, relationship.ContribNone, relationship.ConfidenceHigh),
		}
		c := relationship.ComputeConsensus(votes)
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
		c := relationship.ComputeConsensus(votes)
		Expect(c.Relationship).To(Equal(relationship.RelContributesTo))
		Expect(c.ContributionType).To(Equal(relationship.ContribExampleOf))
	})

	It("returns NO_RELATIONSHIP for empty votes map", func() {
		c := relationship.ComputeConsensus(map[string]*relationship.Vote{})
		Expect(c.Relationship).To(Equal(relationship.RelNoRelationship))
		Expect(c.ConfidenceFraction).To(Equal(0.0))
		Expect(c.ValidVoteCount).To(Equal(0))
		Expect(c.Unanimous).To(BeFalse())
	})

	It("handles single vote as trivially unanimous", func() {
		votes := map[string]*relationship.Vote{
			"qwen3": makeVote(relationship.RelSubsetOf, relationship.ContribNone, relationship.ConfidenceHigh),
		}
		c := relationship.ComputeConsensus(votes)
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
		c := relationship.ComputeConsensus(votes)
		Expect(c.Relationship).To(Equal(relationship.RelContributesTo))
		Expect(c.ContributionType).To(Equal(relationship.ContribIntegralTo))
	})

	It("includes all votes in AllVotes including parse errors", func() {
		votes := map[string]*relationship.Vote{
			"qwen3":   makeVote(relationship.RelEquivalent, relationship.ContribNone, relationship.ConfidenceHigh),
			"mistral": makeErrorVote(relationship.ParseError),
		}
		c := relationship.ComputeConsensus(votes)
		Expect(c.AllVotes).To(HaveLen(2))
		Expect(c.AllVotes).To(ContainElements("EQUIVALENT", "PARSE_ERROR"))
	})
})
