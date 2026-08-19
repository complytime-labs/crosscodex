package pipeline

import (
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
)

func TestBuildSynthesisInputs_RequiresRow(t *testing.T) {
	requires := []results.RequiresResult{{SourceID: "AC-1", TargetID: "AC-2", Confidence: 0.9, Unanimous: true}}
	inputs, _ := buildSynthesisInputs(nil, requires, nil, nil)

	if len(inputs) != 1 {
		t.Fatalf("len(inputs) = %d; want 1", len(inputs))
	}
	got := inputs[0]
	if got.ConsensusRelationship != "requires" || got.ContributionType != "requires" ||
		got.ConfidenceFraction != 0.9 || !got.Unanimous {
		t.Errorf("inputs[0] = %+v; want requires row per spec table", got)
	}
}

func TestBuildSynthesisInputs_RelationshipRow(t *testing.T) {
	rel := []results.SemanticMatchResult{{SourceID: "AC-1", TargetID: "AC-3", RelationshipType: "supports", Confidence: 0.7}}
	inputs, _ := buildSynthesisInputs(nil, nil, rel, nil)

	if len(inputs) != 1 {
		t.Fatalf("len(inputs) = %d; want 1", len(inputs))
	}
	got := inputs[0]
	if got.ConsensusRelationship != "supports" || got.ContributionType != "relationship" ||
		got.ConfidenceFraction != 0.7 || !got.Unanimous {
		t.Errorf("inputs[0] = %+v; want relationship row with Unanimous=true per spec table", got)
	}
}

func TestBuildSynthesisInputs_TwoRowsPerPair(t *testing.T) {
	requires := []results.RequiresResult{{SourceID: "AC-1", TargetID: "AC-2", Confidence: 0.9}}
	rel := []results.SemanticMatchResult{{SourceID: "AC-1", TargetID: "AC-2", RelationshipType: "supports", Confidence: 0.7}}
	inputs, _ := buildSynthesisInputs(nil, requires, rel, nil)

	if len(inputs) != 2 {
		t.Fatalf("len(inputs) = %d; want 2 (same pair, two contribution types)", len(inputs))
	}
}

func TestBuildSynthesisInputs_SimilarityStats(t *testing.T) {
	similarity := map[string]similarityMatrix{
		"model-a": {IDs: []string{"AC-1", "AC-2"}, Values: [][]float32{{0, 80}, {80, 0}}},
		"model-b": {IDs: []string{"AC-1", "AC-2"}, Values: [][]float32{{0, 90}, {90, 0}}},
	}
	requires := []results.RequiresResult{{SourceID: "AC-1", TargetID: "AC-2", Confidence: 0.9}}
	inputs, _ := buildSynthesisInputs(nil, requires, nil, similarity)

	got := inputs[0]
	if got.SimilarityCount != 2 {
		t.Errorf("SimilarityCount = %d; want 2", got.SimilarityCount)
	}
	if got.SimilarityScore != 85.0 { // mean(80, 90)
		t.Errorf("SimilarityScore (mean) = %v; want 85.0", got.SimilarityScore)
	}
	if got.SimilarityMedian != 85.0 { // average of the two middle values, 80 and 90
		t.Errorf("SimilarityMedian (even count) = %v; want 85.0", got.SimilarityMedian)
	}
}

func TestBuildSynthesisInputs_SimilarityMedian_OddCount(t *testing.T) {
	similarity := map[string]similarityMatrix{
		"model-a": {IDs: []string{"AC-1", "AC-2"}, Values: [][]float32{{0, 80}, {80, 0}}},
		"model-b": {IDs: []string{"AC-1", "AC-2"}, Values: [][]float32{{0, 85}, {85, 0}}},
		"model-c": {IDs: []string{"AC-1", "AC-2"}, Values: [][]float32{{0, 90}, {90, 0}}},
	}
	requires := []results.RequiresResult{{SourceID: "AC-1", TargetID: "AC-2", Confidence: 0.9}}
	inputs, _ := buildSynthesisInputs(nil, requires, nil, similarity)

	got := inputs[0]
	if got.SimilarityCount != 3 {
		t.Errorf("SimilarityCount = %d; want 3", got.SimilarityCount)
	}
	if got.SimilarityMedian != 85.0 { // middle value of [80, 85, 90]
		t.Errorf("SimilarityMedian (odd count) = %v; want 85.0", got.SimilarityMedian)
	}
}

func TestBuildSynthesisInputs_ClassificationsMap(t *testing.T) {
	classify := []results.ClassifyResult{{ControlID: "AC-1", Type: "Technical", Level: "Operational"}}
	_, classifications := buildSynthesisInputs(classify, nil, nil, nil)

	got, ok := classifications["AC-1"]
	if !ok || got.Type != "Technical" || got.Level != "Operational" {
		t.Errorf("classifications[\"AC-1\"] = %+v, ok=%v; want {Technical Operational}, true", got, ok)
	}
}
