package builtin_test

import (
	"context"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate/builtin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
)

// SimilarityMatrix mirrors internal/analyzer/embedding/types.go for testing
type SimilarityMatrix struct {
	IDs    []string
	Values [][]float32
}

func TestSemanticGenerator_Name(t *testing.T) {
	gen := builtin.NewSemanticGenerator()
	assert.Equal(t, "semantic", gen.Name())
}

func TestSemanticGenerator_Generate_TopK(t *testing.T) {
	gen := builtin.NewSemanticGenerator()

	// Create a 3x3 similarity matrix
	// AC-1: [100, 80, 60]
	// AC-2: [80, 100, 70]
	// AC-3: [60, 70, 100]
	matrix := &SimilarityMatrix{
		IDs: []string{"AC-1", "AC-2", "AC-3"},
		Values: [][]float32{
			{100, 80, 60},
			{80, 100, 70},
			{60, 70, 100},
		},
	}

	sources := map[string]*candidate.ControlData{
		"AC-1": {ControlID: "AC-1", Text: "Source control 1"},
	}

	targets := map[string]*candidate.ControlData{
		"AC-2": {ControlID: "AC-2", Text: "Target control 2"},
		"AC-3": {ControlID: "AC-3", Text: "Target control 3"},
	}

	req := candidate.GenerateRequest{
		TenantID:        "test-tenant",
		JobID:           "test-job",
		SourceControls:  sources,
		TargetControls:  targets,
		EmbeddingMatrix: matrix,
		Parameters: map[string]interface{}{
			"top_k":          2,
			"min_similarity": 60.0,
			"weight":         1.0,
		},
	}

	candidates, err := gen.Generate(context.Background(), req)
	require.NoError(t, err)

	// Should return top-2 most similar targets for AC-1
	// AC-2 (similarity 80) and AC-3 (similarity 60)
	require.Len(t, candidates, 2)

	// Sort by score descending
	if candidates[0].Score < candidates[1].Score {
		candidates[0], candidates[1] = candidates[1], candidates[0]
	}

	// First should be AC-2 with score 0.80 (80/100)
	assert.Equal(t, "AC-1", candidates[0].SourceID)
	assert.Equal(t, "AC-2", candidates[0].TargetID)
	assert.InDelta(t, 0.80, candidates[0].Score, 0.01)
	assert.Equal(t, 1.0, candidates[0].Weight)
	assert.Equal(t, "semantic", candidates[0].GeneratorID)

	// Second should be AC-3 with score 0.60
	assert.Equal(t, "AC-1", candidates[1].SourceID)
	assert.Equal(t, "AC-3", candidates[1].TargetID)
	assert.InDelta(t, 0.60, candidates[1].Score, 0.01)
	assert.Equal(t, 1.0, candidates[1].Weight)
	assert.Equal(t, "semantic", candidates[1].GeneratorID)
}

func TestSemanticGenerator_Generate_MinSimilarityFilter(t *testing.T) {
	gen := builtin.NewSemanticGenerator()

	matrix := &SimilarityMatrix{
		IDs: []string{"AC-1", "AC-2", "AC-3"},
		Values: [][]float32{
			{100, 80, 40}, // AC-3 below threshold
			{80, 100, 70},
			{40, 70, 100},
		},
	}

	sources := map[string]*candidate.ControlData{
		"AC-1": {ControlID: "AC-1", Text: "Source control 1"},
	}

	targets := map[string]*candidate.ControlData{
		"AC-2": {ControlID: "AC-2", Text: "Target control 2"},
		"AC-3": {ControlID: "AC-3", Text: "Target control 3"},
	}

	req := candidate.GenerateRequest{
		TenantID:        "test-tenant",
		JobID:           "test-job",
		SourceControls:  sources,
		TargetControls:  targets,
		EmbeddingMatrix: matrix,
		Parameters: map[string]interface{}{
			"top_k":          10,
			"min_similarity": 50.0, // Filter out AC-3 (40)
			"weight":         0.8,
		},
	}

	candidates, err := gen.Generate(context.Background(), req)
	require.NoError(t, err)

	// Should only return AC-2 (80 >= 50)
	require.Len(t, candidates, 1)
	assert.Equal(t, "AC-2", candidates[0].TargetID)
	assert.InDelta(t, 0.80, candidates[0].Score, 0.01)
	assert.Equal(t, 0.8, candidates[0].Weight)
}

func TestSemanticGenerator_Generate_NoEmbeddingMatrix(t *testing.T) {
	gen := builtin.NewSemanticGenerator()

	req := candidate.GenerateRequest{
		TenantID:       "test-tenant",
		JobID:          "test-job",
		SourceControls: map[string]*candidate.ControlData{},
		TargetControls: map[string]*candidate.ControlData{},
		// No EmbeddingMatrix provided
		Parameters: map[string]interface{}{
			"top_k":          10,
			"min_similarity": 50.0,
			"weight":         1.0,
		},
	}

	candidates, err := gen.Generate(context.Background(), req)
	require.NoError(t, err)
	assert.Empty(t, candidates, "Should return empty slice when no embedding matrix")
}

func TestSemanticGenerator_Generate_MultipleSourcesAllTargets(t *testing.T) {
	gen := builtin.NewSemanticGenerator()

	// 4x4 matrix: 2 sources (AC-1, AC-2) and 2 targets (AC-3, AC-4)
	matrix := &SimilarityMatrix{
		IDs: []string{"AC-1", "AC-2", "AC-3", "AC-4"},
		Values: [][]float32{
			{100, 50, 90, 70},
			{50, 100, 60, 85},
			{90, 60, 100, 55},
			{70, 85, 55, 100},
		},
	}

	sources := map[string]*candidate.ControlData{
		"AC-1": {ControlID: "AC-1", Text: "Source 1"},
		"AC-2": {ControlID: "AC-2", Text: "Source 2"},
	}

	targets := map[string]*candidate.ControlData{
		"AC-3": {ControlID: "AC-3", Text: "Target 3"},
		"AC-4": {ControlID: "AC-4", Text: "Target 4"},
	}

	req := candidate.GenerateRequest{
		TenantID:        "test-tenant",
		JobID:           "test-job",
		SourceControls:  sources,
		TargetControls:  targets,
		EmbeddingMatrix: matrix,
		Parameters: map[string]interface{}{
			"top_k":          1, // Only top-1 per source
			"min_similarity": 60.0,
			"weight":         1.0,
		},
	}

	candidates, err := gen.Generate(context.Background(), req)
	require.NoError(t, err)

	// Should have 2 candidates: top-1 for AC-1 and top-1 for AC-2
	// AC-1 -> AC-3 (90)
	// AC-2 -> AC-4 (85)
	require.Len(t, candidates, 2)

	// Find each candidate
	var ac1Candidate, ac2Candidate *candidate.Candidate
	for i := range candidates {
		if candidates[i].SourceID == "AC-1" {
			ac1Candidate = &candidates[i]
		} else if candidates[i].SourceID == "AC-2" {
			ac2Candidate = &candidates[i]
		}
	}

	require.NotNil(t, ac1Candidate)
	assert.Equal(t, "AC-3", ac1Candidate.TargetID)
	assert.InDelta(t, 0.90, ac1Candidate.Score, 0.01)

	require.NotNil(t, ac2Candidate)
	assert.Equal(t, "AC-4", ac2Candidate.TargetID)
	assert.InDelta(t, 0.85, ac2Candidate.Score, 0.01)
}

func TestSemanticGenerator_WithTelemetry(t *testing.T) {
	// Create noop tracer for testing
	tracer := noop.NewTracerProvider().Tracer("test")
	gen := builtin.NewSemanticGenerator(builtin.WithSemanticTelemetry(tracer))

	matrix := &SimilarityMatrix{
		IDs: []string{"AC-1", "AC-2"},
		Values: [][]float32{
			{100, 80},
			{80, 100},
		},
	}

	sources := map[string]*candidate.ControlData{
		"AC-1": {ControlID: "AC-1", Text: "Source"},
	}

	targets := map[string]*candidate.ControlData{
		"AC-2": {ControlID: "AC-2", Text: "Target"},
	}

	req := candidate.GenerateRequest{
		TenantID:        "test-tenant",
		SourceControls:  sources,
		TargetControls:  targets,
		EmbeddingMatrix: matrix,
		Parameters: map[string]interface{}{
			"top_k": 1,
		},
	}

	candidates, err := gen.Generate(context.Background(), req)
	require.NoError(t, err)
	assert.NotEmpty(t, candidates)
	assert.Len(t, candidates, 1)
}
