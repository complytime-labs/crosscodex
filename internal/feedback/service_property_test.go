//go:build !integration

package feedback_test

import (
	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/internal/feedback"
)

var _ = Describe("Property Specifications", Ordered, func() {
	It("aggregation counts every vote once, scores agreement in [0, 1] and escalates exactly as the rule says", func() {
		types := []feedback.VoteType{feedback.VoteAgree, feedback.VoteDisagree, feedback.VoteRefine, feedback.VoteEscalate}
		suggestions := []string{"EQUIVALENT", "SUBSET_OF", "SUPERSET_OF", "PARTIAL"}
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			n := rapid.IntRange(0, 40).Draw(t, "votes")
			votes := make([]feedback.Vote, n)
			for i := range votes {
				votes[i].Type = rapid.SampledFrom(types).Draw(t, "type")
				if votes[i].Type == feedback.VoteRefine {
					votes[i].SuggestedType = rapid.SampledFrom(suggestions).Draw(t, "suggested")
				}
			}
			minAgreement := rapid.Float64Range(0, 1).Draw(t, "min_agreement")
			a := feedback.Aggregate(feedback.Mapping{RelationshipType: "EQUIVALENT"}, votes, minAgreement)

			if a.Votes() != n {
				t.Fatalf("counts %+v sum to %d, want %d votes", a, a.Votes(), n)
			}
			if a.AgreementScore < 0 || a.AgreementScore > 1 {
				t.Fatalf("agreement score %v is outside [0, 1]", a.AgreementScore)
			}
			if want := a.Escalate > 0 || (n > 0 && a.AgreementScore < minAgreement); a.NeedsEscalation != want {
				t.Fatalf("needs_escalation %v, want %v for %+v at min_agreement %v", a.NeedsEscalation, want, a, minAgreement)
			}
			if a.Refine <= max(a.Agree, a.Disagree, a.Escalate) && a.ConsensusType != "EQUIVALENT" {
				t.Fatalf("consensus %q without refine votes outnumbering every other type: %+v", a.ConsensusType, a)
			}
		})
	})
})
