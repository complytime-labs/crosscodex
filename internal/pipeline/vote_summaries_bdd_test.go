package pipeline

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
)

var _ = Describe("buildVoteSummaryPairs", func() {
	It("merges requires and relationship results, deduping on (source, target)", func() {
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

		// AC-1--AC-2 appears in both sources but must produce exactly one
		// vote_summaries row (PK is job_id/source_id/target_id) -- relationship's
		// type wins as the consensus label when both are present.
		Expect(pairs).To(HaveLen(2), "AC-1--AC-2 deduped, AC-3--AC-4")
		byPair := make(map[[2]string]VoteSummaryPair)
		for _, p := range pairs {
			byPair[[2]string{p.SourceID, p.TargetID}] = p
		}
		Expect(byPair[[2]string{"AC-1", "AC-2"}].Consensus).To(Equal("supports"))
		Expect(byPair[[2]string{"AC-3", "AC-4"}].Consensus).To(Equal("conflicts"))
	})

	It("handles requires-only output", func() {
		requiresJSON, _ := json.Marshal([]results.RequiresResult{{SourceID: "AC-1", TargetID: "AC-2", Confidence: 0.9}})
		outputs := map[string]*analyzer.Output{"requires": {ResultData: requiresJSON}}

		pairs, err := buildVoteSummaryPairs(outputs)
		Expect(err).NotTo(HaveOccurred())
		Expect(pairs).To(HaveLen(1))
		Expect(pairs[0].Consensus).To(Equal("requires"))
	})
})
