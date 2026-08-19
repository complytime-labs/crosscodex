package results_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
)

func TestRequiresResultRoundTrip(t *testing.T) {
	want := []results.RequiresResult{{
		SourceID: "AC-1", TargetID: "AC-2", Confidence: 0.8,
		Unanimous: true, ValidVotes: 3, TotalVotes: 3, Models: []string{"llama3.2:3b"},
	}}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got []results.RequiresResult
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got[0], want[0]) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got[0], want[0])
	}
}

func TestSemanticMatchResultRoundTrip(t *testing.T) {
	want := []results.SemanticMatchResult{{
		SourceID:         "AC-1",
		TargetID:         "AC-2",
		RelationshipType: "SUPPORTS",
		Confidence:       0.9,
		Properties:       map[string]string{"note": "derived"},
	}}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got []results.SemanticMatchResult
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got[0], want[0]) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got[0], want[0])
	}
}

func TestArtifactResultRoundTrip(t *testing.T) {
	want := []results.ArtifactResult{{
		ControlID: "AC-1",
		Artifacts: []results.Artifact{{
			Name:       "Security Log",
			Type:       "log",
			Frequency:  "daily",
			OwnerRole:  "admin",
			Confidence: 0.7,
		}},
	}}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got []results.ArtifactResult
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got[0].ControlID != want[0].ControlID || got[0].Artifacts[0] != want[0].Artifacts[0] {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got[0], want[0])
	}
}

func TestClassifyResultRoundTrip(t *testing.T) {
	want := []results.ClassifyResult{{
		ControlID: "AC-1",
		Type:      "technical",
		Level:     "high",
	}}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got []results.ClassifyResult
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got[0] != want[0] {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got[0], want[0])
	}
}

func TestEmbedResultRoundTrip(t *testing.T) {
	want := []results.EmbedResult{{
		ControlID: "AC-1",
	}}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got []results.EmbedResult
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got[0] != want[0] {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got[0], want[0])
	}
}
