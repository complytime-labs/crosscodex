package pipeline

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/synthesis"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
)

var _ = Describe("buildVoteSummaryPairs", func() {
	It("emits one row per analyzer contribution, keeping a pair both analyzers produced as two rows", func() {
		requiresJSON, _ := json.Marshal([]results.RequiresResult{
			{SourceID: "AC-1", TargetID: "AC-2", Confidence: 0.9},
		})
		relJSON, _ := json.Marshal([]results.SemanticMatchResult{
			{SourceID: "AC-1", TargetID: "AC-2", RelationshipType: "supports", Confidence: 0.8},
			{SourceID: "AC-3", TargetID: "AC-4", RelationshipType: "conflicts", Confidence: 0.7},
		})
		outputs := map[string]*analyzer.Output{
			"requires":     {AnalyzerName: "requires", ResultData: requiresJSON},
			"relationship": {AnalyzerName: "relationship", ResultData: relJSON},
		}

		pairs, err := buildVoteSummaryPairs(outputs)
		Expect(err).NotTo(HaveOccurred())

		// buildSynthesisInputs emits one synthesis row per contribution, and
		// synthesis updates one vote_summaries row per synthesis row, so
		// AC-1--AC-2 must keep both its requires and relationship rows.
		Expect(pairs).To(ConsistOf(
			VoteSummaryPair{SourceID: "AC-1", TargetID: "AC-2", Analyzer: "requires", Consensus: synthesis.ConsensusRequires, Confidence: 0.9},
			VoteSummaryPair{SourceID: "AC-1", TargetID: "AC-2", Analyzer: "relationship", Consensus: "supports", Confidence: 0.8},
			VoteSummaryPair{SourceID: "AC-3", TargetID: "AC-4", Analyzer: "relationship", Consensus: "conflicts", Confidence: 0.7},
		))
	})

	It("handles requires-only output", func() {
		requiresJSON, _ := json.Marshal([]results.RequiresResult{{SourceID: "AC-1", TargetID: "AC-2", Confidence: 0.9}})
		outputs := map[string]*analyzer.Output{"requires": {ResultData: requiresJSON}}

		pairs, err := buildVoteSummaryPairs(outputs)
		Expect(err).NotTo(HaveOccurred())
		Expect(pairs).To(Equal([]VoteSummaryPair{
			{SourceID: "AC-1", TargetID: "AC-2", Analyzer: "requires", Consensus: synthesis.ConsensusRequires, Confidence: 0.9},
		}))
	})

	It("returns no rows when neither analyzer produced output", func() {
		pairs, err := buildVoteSummaryPairs(map[string]*analyzer.Output{})
		Expect(err).NotTo(HaveOccurred())
		Expect(pairs).To(BeEmpty())
	})

	DescribeTable("rejects one analyzer listing the same pair twice as corrupt results",
		func(analyzerName string, data any) {
			raw, err := json.Marshal(data)
			Expect(err).NotTo(HaveOccurred())
			outputs := map[string]*analyzer.Output{analyzerName: {AnalyzerName: analyzerName, ResultData: raw}}

			pairs, err := buildVoteSummaryPairs(outputs)
			Expect(err).To(HaveOccurred())
			Expect(pairs).To(BeNil())
			Expect(err.Error()).To(ContainSubstring(analyzerName))
			Expect(err.Error()).To(ContainSubstring("AC-1--AC-2"))
			Expect(err.Error()).To(ContainSubstring("create a new job"))
		},
		Entry("requires", "requires", []results.RequiresResult{
			{SourceID: "AC-1", TargetID: "AC-2", Confidence: 0.9},
			{SourceID: "AC-1", TargetID: "AC-2", Confidence: 0.4},
		}),
		Entry("relationship", "relationship", []results.SemanticMatchResult{
			{SourceID: "AC-1", TargetID: "AC-2", RelationshipType: "supports", Confidence: 0.8},
			{SourceID: "AC-1", TargetID: "AC-2", RelationshipType: "conflicts", Confidence: 0.6},
		}),
	)
})
