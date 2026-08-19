package pipeline

import (
	"encoding/json"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
)

func TestBuildVoteSummaryPairs_MergesRequiresAndRelationship(t *testing.T) {
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
	if err != nil {
		t.Fatalf("buildVoteSummaryPairs() error = %v", err)
	}

	// AC-1--AC-2 appears in both sources but must produce exactly one
	// vote_summaries row (PK is job_id/source_id/target_id) -- relationship's
	// type wins as the consensus label when both are present.
	if len(pairs) != 2 {
		t.Fatalf("len(pairs) = %d; want 2 (AC-1--AC-2 deduped, AC-3--AC-4)", len(pairs))
	}
	byPair := make(map[[2]string]VoteSummaryPair)
	for _, p := range pairs {
		byPair[[2]string{p.SourceID, p.TargetID}] = p
	}
	if got := byPair[[2]string{"AC-1", "AC-2"}]; got.Consensus != "supports" {
		t.Errorf("AC-1--AC-2 consensus = %q; want \"supports\"", got.Consensus)
	}
	if got := byPair[[2]string{"AC-3", "AC-4"}]; got.Consensus != "conflicts" {
		t.Errorf("AC-3--AC-4 consensus = %q; want \"conflicts\"", got.Consensus)
	}
}

func TestBuildVoteSummaryPairs_RequiresOnly(t *testing.T) {
	requiresJSON, _ := json.Marshal([]results.RequiresResult{{SourceID: "AC-1", TargetID: "AC-2", Confidence: 0.9}})
	outputs := map[string]*analyzer.Output{"requires": {ResultData: requiresJSON}}

	pairs, err := buildVoteSummaryPairs(outputs)
	if err != nil {
		t.Fatalf("buildVoteSummaryPairs() error = %v", err)
	}
	if len(pairs) != 1 || pairs[0].Consensus != "requires" {
		t.Errorf("pairs = %+v; want one pair with consensus \"requires\"", pairs)
	}
}
