package consensus_test

import (
	"errors"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/consensus"
)

func TestConsensusBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Consensus BDD Suite")
}

var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

var _ = Describe("Consensus Computer", func() {
	Describe("simple majority voting", func() {
		It("decides true when majority votes yes", func() {
			computer := consensus.New()

			yes, no := true, false
			votes := []consensus.Vote{
				{VoterID: "m1", Decision: &yes, Weight: 1.0},
				{VoterID: "m2", Decision: &yes, Weight: 1.0},
				{VoterID: "m3", Decision: &no, Weight: 1.0},
			}

			result, err := computer.Compute(votes)
			Expect(err).NotTo(HaveOccurred())

			Expect(result.Decision).To(BeTrue())
			Expect(result.ConfidenceFraction).To(Equal(2.0 / 3.0))
			Expect(result.Unanimous).To(BeFalse())
			Expect(result.ValidVoteCount).To(Equal(3))
			Expect(result.TotalVoteCount).To(Equal(3))
		})
	})

	Describe("unanimous decisions", func() {
		It("reports unanimous when all votes agree", func() {
			computer := consensus.New()

			yes := true
			votes := []consensus.Vote{
				{VoterID: "m1", Decision: &yes, Weight: 1.0},
				{VoterID: "m2", Decision: &yes, Weight: 1.0},
				{VoterID: "m3", Decision: &yes, Weight: 1.0},
			}

			result, err := computer.Compute(votes)
			Expect(err).NotTo(HaveOccurred())

			Expect(result.Decision).To(BeTrue())
			Expect(result.ConfidenceFraction).To(Equal(1.0))
			Expect(result.Unanimous).To(BeTrue())
		})
	})

	Context("when votes are tied", func() {
		It("defaults to false (conservative)", func() {
			computer := consensus.New()

			yes, no := true, false
			votes := []consensus.Vote{
				{VoterID: "m1", Decision: &yes, Weight: 1.0},
				{VoterID: "m2", Decision: &no, Weight: 1.0},
			}

			result, err := computer.Compute(votes)
			Expect(err).NotTo(HaveOccurred())

			Expect(result.Decision).To(BeFalse())
		})
	})

	Context("with weighted votes", func() {
		It("uses weights to compute confidence and handles ties conservatively", func() {
			computer := consensus.New()

			yes, no := true, false
			votes := []consensus.Vote{
				{VoterID: "m1", Decision: &yes, Weight: 2.0},
				{VoterID: "m2", Decision: &no, Weight: 1.0},
				{VoterID: "m3", Decision: &no, Weight: 1.0},
			}

			result, err := computer.Compute(votes)
			Expect(err).NotTo(HaveOccurred())

			// yes=2.0, no=2.0 -> tie -> false
			Expect(result.Decision).To(BeFalse())
			Expect(result.ConfidenceFraction).To(Equal(2.0 / 4.0))
		})
	})

	Context("when minimum valid votes not met", func() {
		It("returns ErrInsufficientVotes", func() {
			computer := consensus.New(consensus.WithMinValidVotes(5))

			yes := true
			votes := []consensus.Vote{
				{VoterID: "m1", Decision: &yes, Weight: 1.0},
				{VoterID: "m2", Decision: &yes, Weight: 1.0},
				{VoterID: "m3", Decision: nil},
			}

			_, err := computer.Compute(votes)
			Expect(err).To(HaveOccurred())

			var insufficientErr *consensus.ErrInsufficientVotes
			Expect(errors.As(err, &insufficientErr)).To(BeTrue())
			Expect(insufficientErr.ValidCount).To(Equal(2))
			Expect(insufficientErr.RequiredMin).To(Equal(5))
		})
	})

	Context("when error rate exceeds threshold", func() {
		It("returns an error", func() {
			computer := consensus.New(consensus.WithMaxErrorRate(0.25))

			yes := true
			votes := []consensus.Vote{
				{VoterID: "m1", Decision: &yes},
				{VoterID: "m2", Decision: &yes},
				{VoterID: "m3", Decision: nil},
				{VoterID: "m4", Decision: nil},
			}

			_, err := computer.Compute(votes)
			Expect(err).To(HaveOccurred())
		})
	})

	Context("when vote weight is zero", func() {
		It("treats Weight=0 as 1.0", func() {
			computer := consensus.New()

			yes, no := true, false
			votes := []consensus.Vote{
				{VoterID: "m1", Decision: &yes, Weight: 0},
				{VoterID: "m2", Decision: &no, Weight: 0},
			}

			result, err := computer.Compute(votes)
			Expect(err).NotTo(HaveOccurred())

			Expect(result.TotalWeight).To(Equal(2.0))
		})
	})
})

var _ = Describe("Consensus Types", func() {
	Describe("Vote", func() {
		It("has Weight field of 0 when not set", func() {
			decision := true
			vote := consensus.Vote{
				VoterID:       "model-1",
				Decision:      &decision,
				Confidence:    "HIGH",
				Weight:        0,
				Justification: "test",
				RawResponse:   "response",
			}

			Expect(vote.Weight).To(Equal(float64(0)))
		})
	})

	Describe("Result", func() {
		It("holds all decision fields", func() {
			result := consensus.Result{
				Decision:           true,
				ConfidenceFraction: 0.75,
				Unanimous:          false,
				ValidVoteCount:     3,
				TotalVoteCount:     4,
				TotalWeight:        3.0,
			}

			Expect(result.Decision).To(BeTrue())
			Expect(result.ConfidenceFraction).To(Equal(0.75))
			Expect(result.Unanimous).To(BeFalse())
		})
	})

	Describe("ErrInsufficientVotes", func() {
		It("formats a descriptive error message", func() {
			err := &consensus.ErrInsufficientVotes{
				ValidCount:  2,
				RequiredMin: 5,
				TotalCount:  10,
				ErrorCount:  8,
			}

			msg := err.Error()
			Expect(msg).To(ContainSubstring("insufficient valid votes"))
			Expect(msg).To(ContainSubstring("2/10"))
			Expect(msg).To(ContainSubstring("required 5"))
		})
	})
})
