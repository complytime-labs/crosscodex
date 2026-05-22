package vectordb

import (
	"testing"
)

func TestEmbeddingValidation(t *testing.T) {
	embedding := Embedding{
		CatalogID: "nist-800-53",
		ControlID: "AC-1",
		Model:     "text-embedding-ada-002",
		Vector:    []float32{0.1, 0.2, 0.3},
		Metadata: map[string]any{
			"oscal.type":   "control",
			"oscal.family": "AC",
		},
	}

	if embedding.CatalogID == "" {
		t.Error("expected non-empty CatalogID")
	}
	if len(embedding.Vector) == 0 {
		t.Error("expected non-empty Vector")
	}
}

func TestFindSimilarQuery(t *testing.T) {
	query := FindSimilarQuery{
		CatalogID: "nist-800-53",
		Model:     "text-embedding-ada-002",
		Vector:    []float32{0.1, 0.2},
		Limit:     10,
	}

	if query.Limit <= 0 {
		t.Error("expected positive Limit")
	}
}
