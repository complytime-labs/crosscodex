package relationship_test

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/internal/analyzer/relationship"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("Property Specifications", Ordered, func() {
	weights := defaultConfidenceWeights

	// Generator: weights that pass config validation, each in (0, 1] with
	// high >= medium >= low.
	genWeights := func(t *rapid.T) config.RelationshipConfidenceWeights {
		low := rapid.Float64Range(0.001, 1).Draw(t, "low")
		medium := rapid.Float64Range(low, 1).Draw(t, "medium")
		high := rapid.Float64Range(medium, 1).Draw(t, "high")
		return config.RelationshipConfidenceWeights{High: high, Medium: medium, Low: low}
	}

	// Generator: random valid vote
	genVote := func(t *rapid.T, label string) *relationship.Vote {
		relIdx := rapid.IntRange(0, 7).Draw(t, label+"_rel")
		contribIdx := rapid.IntRange(0, 2).Draw(t, label+"_contrib")
		confIdx := rapid.IntRange(0, 3).Draw(t, label+"_conf")
		statusIdx := rapid.IntRange(0, 2).Draw(t, label+"_status")
		return &relationship.Vote{
			Relationship:     relationship.RelationshipType(relIdx),
			ContributionType: relationship.ContributionType(contribIdx),
			Confidence:       relationship.ConfidenceLevel(confIdx),
			ParseStatus:      relationship.ParseStatus(statusIdx),
		}
	}

	// Generator: random vote map with 1-10 entries
	genVotes := func(t *rapid.T) map[string]*relationship.Vote {
		n := rapid.IntRange(1, 10).Draw(t, "n_votes")
		votes := make(map[string]*relationship.Vote, n)
		for i := 0; i < n; i++ {
			key := fmt.Sprintf("model_%d", i)
			votes[key] = genVote(t, key)
		}
		return votes
	}

	Context("ComputeConsensus — determinism", func() {
		It("produces identical results for identical inputs", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				votes := genVotes(t)
				c1 := relationship.ComputeConsensus(votes, weights)
				c2 := relationship.ComputeConsensus(votes, weights)
				if c1.Relationship != c2.Relationship {
					t.Fatalf("non-deterministic: %v vs %v", c1.Relationship, c2.Relationship)
				}
				if c1.ConfidenceFraction != c2.ConfidenceFraction {
					t.Fatalf("non-deterministic confidence: %v vs %v", c1.ConfidenceFraction, c2.ConfidenceFraction)
				}
				if c1.Unanimous != c2.Unanimous {
					t.Fatalf("non-deterministic unanimous: %v vs %v", c1.Unanimous, c2.Unanimous)
				}
				if c1.ContributionType != c2.ContributionType {
					t.Fatalf("non-deterministic contribution: %v vs %v", c1.ContributionType, c2.ContributionType)
				}
			})
		})
	})

	Context("ComputeConsensus — valid output", func() {
		It("always returns a valid RelationshipType", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				votes := genVotes(t)
				c := relationship.ComputeConsensus(votes, weights)
				if !c.Relationship.Valid() {
					t.Fatalf("invalid relationship: %v", c.Relationship)
				}
			})
		})
	})

	Context("ComputeConsensus — confidence range", func() {
		It("confidence is always in [0.0, 1.0]", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				votes := genVotes(t)
				c := relationship.ComputeConsensus(votes, weights)
				if c.ConfidenceFraction < 0 || c.ConfidenceFraction > 1.0 {
					t.Fatalf("confidence out of range: %v", c.ConfidenceFraction)
				}
			})
		})
	})

	Context("ComputeConsensus — confidence is agreement scaled by the winners' weights", func() {
		It("lies between agreement x low and agreement x high, and is positive whenever a vote is valid", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				votes := genVotes(t)
				w := genWeights(t)
				c := relationship.ComputeConsensus(votes, w)
				if c.ValidVoteCount == 0 {
					if c.ConfidenceFraction != 0 || c.AgreementFraction != 0 {
						t.Fatalf("no valid votes: confidence %v, agreement %v, want 0", c.ConfidenceFraction, c.AgreementFraction)
					}
					return
				}
				// Both values are rounded to 3 decimals.
				const tol = 0.001
				lo, hi := c.AgreementFraction*w.Low, c.AgreementFraction*w.High
				if c.ConfidenceFraction < lo-tol || c.ConfidenceFraction > hi+tol {
					t.Fatalf("confidence %v outside [%v, %v] (agreement %v, weights %+v)", c.ConfidenceFraction, lo, hi, c.AgreementFraction, w)
				}
				if c.ConfidenceFraction <= 0 && lo >= 0.0005 {
					t.Fatalf("confidence %v not positive with %d valid votes (weights %+v)", c.ConfidenceFraction, c.ValidVoteCount, w)
				}
			})
		})
	})

	Context("ComputeConsensus — unanimous correctness", func() {
		It("unanimous is true iff all valid votes have the same type", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				votes := genVotes(t)
				c := relationship.ComputeConsensus(votes, weights)

				// Count distinct valid relationship types
				seen := make(map[relationship.RelationshipType]bool)
				validCount := 0
				for _, v := range votes {
					if v.ParseStatus == relationship.ParseOK {
						seen[v.Relationship] = true
						validCount++
					}
				}

				if validCount == 0 {
					if c.Unanimous {
						t.Fatalf("unanimous should be false when no valid votes")
					}
				} else if len(seen) == 1 {
					if !c.Unanimous {
						t.Fatalf("unanimous should be true when all valid votes agree")
					}
				} else {
					if c.Unanimous {
						t.Fatalf("unanimous should be false when valid votes disagree")
					}
				}
			})
		})
	})

	Context("ComputeConsensus — valid vote count", func() {
		It("equals input count minus parse errors/fails", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				votes := genVotes(t)
				c := relationship.ComputeConsensus(votes, weights)

				expectedValid := 0
				for _, v := range votes {
					if v.ParseStatus == relationship.ParseOK {
						expectedValid++
					}
				}
				if c.ValidVoteCount != expectedValid {
					t.Fatalf("valid count %d != expected %d", c.ValidVoteCount, expectedValid)
				}
			})
		})
	})

	Context("ComputeConsensus — priority tiebreak stability", func() {
		It("for any tied pair, lower priority index always wins", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				idx1 := rapid.IntRange(0, 7).Draw(t, "type1")
				idx2 := rapid.IntRange(0, 7).Draw(t, "type2")
				if idx1 == idx2 {
					t.Skip("same type, no tiebreak")
				}
				rt1 := relationship.RelationshipType(idx1)
				rt2 := relationship.RelationshipType(idx2)

				votes := map[string]*relationship.Vote{
					"a": {Relationship: rt1, ParseStatus: relationship.ParseOK},
					"b": {Relationship: rt2, ParseStatus: relationship.ParseOK},
				}
				c := relationship.ComputeConsensus(votes, weights)

				expectedWinner := rt1
				if idx2 < idx1 {
					expectedWinner = rt2
				}
				if c.Relationship != expectedWinner {
					t.Fatalf("tiebreak: expected %v, got %v", expectedWinner, c.Relationship)
				}
			})
		})
	})

	Context("ParseResponse — roundtrip", func() {
		It("formats a canonical response and parses back to original fields", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				relIdx := rapid.IntRange(0, 7).Draw(t, "rel")
				rel := relationship.RelationshipType(relIdx)
				confIdx := rapid.IntRange(1, 3).Draw(t, "conf") // 1-3, skip Unknown
				conf := relationship.ConfidenceLevel(confIdx)

				raw := fmt.Sprintf("RELATIONSHIP: %s\nJUSTIFICATION: test reason\nCONFIDENCE: %s",
					rel.String(), conf.String())
				vote := relationship.ParseResponse(raw)

				if vote.ParseStatus != relationship.ParseOK {
					t.Fatalf("failed to parse canonical response for %s", rel.String())
				}
				if vote.Relationship != rel {
					t.Fatalf("roundtrip: expected %v, got %v", rel, vote.Relationship)
				}
				if vote.Confidence != conf {
					t.Fatalf("roundtrip confidence: expected %v, got %v", conf, vote.Confidence)
				}
			})
		})
	})

	Context("ParseResponse — fail-closed", func() {
		It("random bytes never produce a valid relationship other than by coincidence", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				raw := rapid.String().Draw(t, "raw")
				vote := relationship.ParseResponse(raw)
				// The vote must not panic. If it parses OK, the relationship
				// must be valid. If it doesn't parse, status must be ParseFail.
				if vote.ParseStatus == relationship.ParseOK {
					if !vote.Relationship.Valid() {
						t.Fatalf("parsed OK but invalid relationship: %v", vote.Relationship)
					}
				}
			})
		})
	})
})

// Suppress unused import warning for gomega — property tests use rapid assertions
// but gomega is needed for the Ginkgo bootstrap in the same package.
var _ = Expect
