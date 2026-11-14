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

		pairs, err := buildVoteSummaryPairs(outputs, "job-1")
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

		pairs, err := buildVoteSummaryPairs(outputs, "job-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(pairs).To(Equal([]VoteSummaryPair{
			{SourceID: "AC-1", TargetID: "AC-2", Analyzer: "requires", Consensus: synthesis.ConsensusRequires, Confidence: 0.9},
		}))
	})

	It("drops the pairs the critique vetoed, each kind independently", func() {
		requiresJSON, _ := json.Marshal([]results.RequiresResult{
			{SourceID: "AC-1", TargetID: "AC-2", Confidence: 0.9}, {SourceID: "AC-3", TargetID: "AC-4", Confidence: 0.9}})
		relJSON, _ := json.Marshal([]results.SemanticMatchResult{
			{SourceID: "AC-1", TargetID: "AC-2", RelationshipType: "SUBSET_OF", Confidence: 0.8},
			{SourceID: "AC-3", TargetID: "AC-4", RelationshipType: "SUBSET_OF", Confidence: 0.8}})
		critJSON, _ := json.Marshal([]results.CritiqueResult{
			{Kind: results.RelationshipAnalyzer, SourceControlID: "AC-1", TargetControlID: "AC-2", Rejected: 3, Outcome: results.CritiqueVetoed, Vetoed: true},
			{Kind: results.RequiresAnalyzer, SourceControlID: "AC-3", TargetControlID: "AC-4", Rejected: 3, Outcome: results.CritiqueVetoed, Vetoed: true},
			{Kind: results.RequiresAnalyzer, SourceControlID: "AC-1", TargetControlID: "AC-2", Upheld: 2, Rejected: 1, Outcome: results.CritiqueSplit}})
		pairs, err := buildVoteSummaryPairs(map[string]*analyzer.Output{
			results.RequiresAnalyzer:     {ResultData: requiresJSON},
			results.RelationshipAnalyzer: {ResultData: relJSON},
			results.CritiqueAnalyzer:     {ResultData: critJSON},
		}, "job-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(pairs).To(ConsistOf(
			VoteSummaryPair{SourceID: "AC-1", TargetID: "AC-2", Analyzer: "requires", Consensus: synthesis.ConsensusRequires, Confidence: 0.9},
			VoteSummaryPair{SourceID: "AC-3", TargetID: "AC-4", Analyzer: "relationship", Consensus: "SUBSET_OF", Confidence: 0.8},
		))
	})

	It("keeps the pairs the critique rejected without veto", func() {
		requiresJSON, _ := json.Marshal([]results.RequiresResult{{SourceID: "AC-3", TargetID: "AC-4", Confidence: 0.9}})
		relJSON, _ := json.Marshal([]results.SemanticMatchResult{{SourceID: "AC-1", TargetID: "AC-2", RelationshipType: "SUBSET_OF", Confidence: 0.8}})
		critJSON, _ := json.Marshal([]results.CritiqueResult{
			{Kind: results.RelationshipAnalyzer, SourceControlID: "AC-1", TargetControlID: "AC-2", Rejected: 3, Outcome: results.CritiqueRejected},
			{Kind: results.RequiresAnalyzer, SourceControlID: "AC-3", TargetControlID: "AC-4", Rejected: 3, Outcome: results.CritiqueRejected}})
		pairs, err := buildVoteSummaryPairs(map[string]*analyzer.Output{
			results.RequiresAnalyzer:     {ResultData: requiresJSON},
			results.RelationshipAnalyzer: {ResultData: relJSON},
			results.CritiqueAnalyzer:     {ResultData: critJSON},
		}, "job-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(pairs).To(ConsistOf(
			VoteSummaryPair{SourceID: "AC-3", TargetID: "AC-4", Analyzer: "requires", Consensus: synthesis.ConsensusRequires, Confidence: 0.9},
			VoteSummaryPair{SourceID: "AC-1", TargetID: "AC-2", Analyzer: "relationship", Consensus: "SUBSET_OF", Confidence: 0.8},
		))
	})

	It("rejects a critique result that does not decode as corrupt, naming the remedy", func() {
		relJSON, _ := json.Marshal([]results.SemanticMatchResult{{SourceID: "AC-1", TargetID: "AC-2", RelationshipType: "SUBSET_OF"}})
		_, err := buildVoteSummaryPairs(map[string]*analyzer.Output{
			results.RelationshipAnalyzer: {ResultData: relJSON},
			results.CritiqueAnalyzer:     {ResultData: []byte(`{not json`)},
		}, "job-1")
		Expect(err).To(MatchError(results.ErrCorruptCritique))
		Expect(err).To(MatchError(ContainSubstring("the stored critique result of job job-1 does not decode")))
		Expect(err).To(MatchError(ContainSubstring("create a new job")))
	})

	It("returns no rows when neither analyzer produced output", func() {
		pairs, err := buildVoteSummaryPairs(map[string]*analyzer.Output{}, "job-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(pairs).To(BeEmpty())
	})

	DescribeTable("rejects one analyzer listing the same pair twice as corrupt results",
		func(analyzerName string, data any) {
			raw, err := json.Marshal(data)
			Expect(err).NotTo(HaveOccurred())
			outputs := map[string]*analyzer.Output{analyzerName: {AnalyzerName: analyzerName, ResultData: raw}}

			pairs, err := buildVoteSummaryPairs(outputs, "job-1")
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
