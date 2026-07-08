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

func TestKeywordGenerator_Name(t *testing.T) {
	gen := builtin.NewKeywordGenerator()
	assert.Equal(t, "keyword", gen.Name())
}

func TestKeywordGenerator_Generate_FoundationalKeywords(t *testing.T) {
	gen := builtin.NewKeywordGenerator()

	sources := map[string]*candidate.ControlData{
		"AC-1": {ControlID: "AC-1", Text: "Monitor access to systems"},
		"AC-2": {ControlID: "AC-2", Text: "Enforce access policies"},
	}

	targets := map[string]*candidate.ControlData{
		"AC-10": {
			ControlID: "AC-10",
			Text:      "The organization shall define and document access control policy",
		},
		"AC-11": {
			ControlID: "AC-11",
			Text:      "Establish a baseline for system configuration",
		},
		"AC-12": {
			ControlID: "AC-12",
			Text:      "Review access logs quarterly",
		},
	}

	req := candidate.GenerateRequest{
		TenantID:       "test-tenant",
		JobID:          "test-job",
		SourceControls: sources,
		TargetControls: targets,
		Parameters: map[string]interface{}{
			"keywords": []interface{}{"policy", "baseline", "define", "establish"},
			"weight":   0.8,
		},
	}

	candidates, err := gen.Generate(context.Background(), req)
	require.NoError(t, err)

	// Should find AC-10 (contains "define", "policy") and AC-11 (contains "establish", "baseline")
	// Each source should be paired with each foundational target
	// 2 sources × 2 foundational targets = 4 candidates
	require.Len(t, candidates, 4)

	// Check that we have pairs for each source with foundational targets
	hasPair := func(sourceID, targetID string) bool {
		for _, c := range candidates {
			if c.SourceID == sourceID && c.TargetID == targetID {
				return true
			}
		}
		return false
	}

	assert.True(t, hasPair("AC-1", "AC-10"))
	assert.True(t, hasPair("AC-1", "AC-11"))
	assert.True(t, hasPair("AC-2", "AC-10"))
	assert.True(t, hasPair("AC-2", "AC-11"))

	// Verify metadata and scoring
	for _, c := range candidates {
		assert.Equal(t, 1.0, c.Score, "Keyword match should have score 1.0")
		assert.Equal(t, 0.8, c.Weight)
		assert.Equal(t, "keyword", c.GeneratorID)
		assert.NotEmpty(t, c.Metadata["keywords_matched"])
	}
}

func TestKeywordGenerator_Generate_CaseInsensitive(t *testing.T) {
	gen := builtin.NewKeywordGenerator()

	sources := map[string]*candidate.ControlData{
		"AC-1": {ControlID: "AC-1", Text: "Some control"},
	}

	targets := map[string]*candidate.ControlData{
		"AC-10": {
			ControlID: "AC-10",
			Text:      "The organization shall DEFINE and document POLICY",
		},
	}

	req := candidate.GenerateRequest{
		TenantID:       "test-tenant",
		JobID:          "test-job",
		SourceControls: sources,
		TargetControls: targets,
		Parameters: map[string]interface{}{
			"keywords": []interface{}{"policy", "define"},
			"weight":   1.0,
		},
	}

	candidates, err := gen.Generate(context.Background(), req)
	require.NoError(t, err)

	require.Len(t, candidates, 1)
	assert.Equal(t, "AC-1", candidates[0].SourceID)
	assert.Equal(t, "AC-10", candidates[0].TargetID)
}

func TestKeywordGenerator_Generate_NoKeywordsFound(t *testing.T) {
	gen := builtin.NewKeywordGenerator()

	sources := map[string]*candidate.ControlData{
		"AC-1": {ControlID: "AC-1", Text: "Some control"},
	}

	targets := map[string]*candidate.ControlData{
		"AC-10": {
			ControlID: "AC-10",
			Text:      "This target has no foundational keywords",
		},
	}

	req := candidate.GenerateRequest{
		TenantID:       "test-tenant",
		JobID:          "test-job",
		SourceControls: sources,
		TargetControls: targets,
		Parameters: map[string]interface{}{
			"keywords": []interface{}{"policy", "procedure", "baseline"},
			"weight":   1.0,
		},
	}

	candidates, err := gen.Generate(context.Background(), req)
	require.NoError(t, err)
	assert.Empty(t, candidates, "Should return empty when no targets match keywords")
}

func TestKeywordGenerator_Generate_NoKeywordsParameter(t *testing.T) {
	gen := builtin.NewKeywordGenerator()

	req := candidate.GenerateRequest{
		TenantID:       "test-tenant",
		JobID:          "test-job",
		SourceControls: map[string]*candidate.ControlData{},
		TargetControls: map[string]*candidate.ControlData{},
		Parameters:     map[string]interface{}{}, // No keywords parameter
	}

	candidates, err := gen.Generate(context.Background(), req)
	require.NoError(t, err)
	assert.Empty(t, candidates, "Should return empty when no keywords configured")
}

func TestKeywordGenerator_Generate_PartialMatch(t *testing.T) {
	gen := builtin.NewKeywordGenerator()

	sources := map[string]*candidate.ControlData{
		"AC-1": {ControlID: "AC-1", Text: "Source control"},
	}

	targets := map[string]*candidate.ControlData{
		"AC-10": {
			ControlID: "AC-10",
			Text:      "Policies and procedures for access control", // Contains "procedures" (plural)
		},
	}

	req := candidate.GenerateRequest{
		TenantID:       "test-tenant",
		JobID:          "test-job",
		SourceControls: sources,
		TargetControls: targets,
		Parameters: map[string]interface{}{
			"keywords": []interface{}{"procedure"}, // Singular form
			"weight":   1.0,
		},
	}

	candidates, err := gen.Generate(context.Background(), req)
	require.NoError(t, err)

	// Should match "procedures" when searching for "procedure"
	require.Len(t, candidates, 1)
	assert.Equal(t, "AC-10", candidates[0].TargetID)
}

func TestKeywordGenerator_Generate_DefaultKeywords(t *testing.T) {
	gen := builtin.NewKeywordGenerator()

	sources := map[string]*candidate.ControlData{
		"AC-1": {ControlID: "AC-1", Text: "Source"},
	}

	targets := map[string]*candidate.ControlData{
		"AC-10": {ControlID: "AC-10", Text: "Define the security policy"},
		"AC-11": {ControlID: "AC-11", Text: "Review access logs"},
	}

	req := candidate.GenerateRequest{
		TenantID:       "test-tenant",
		JobID:          "test-job",
		SourceControls: sources,
		TargetControls: targets,
		Parameters: map[string]interface{}{
			"weight": 1.0,
			// No keywords parameter - should use defaults
		},
	}

	candidates, err := gen.Generate(context.Background(), req)
	require.NoError(t, err)

	// Should use default keywords and find AC-10 (contains "policy" and "define")
	require.Len(t, candidates, 1)
	assert.Equal(t, "AC-10", candidates[0].TargetID)
}

func TestKeywordGenerator_WithTelemetry(t *testing.T) {
	// Create noop tracer for testing
	tracer := noop.NewTracerProvider().Tracer("test")
	gen := builtin.NewKeywordGenerator(builtin.WithKeywordTelemetry(tracer))

	sources := map[string]*candidate.ControlData{
		"SRC-1": {ControlID: "SRC-1", Text: "Any source"},
	}

	targets := map[string]*candidate.ControlData{
		"TGT-1": {ControlID: "TGT-1", Text: "Establish a policy"},
	}

	req := candidate.GenerateRequest{
		TenantID:       "test-tenant",
		SourceControls: sources,
		TargetControls: targets,
		Parameters:     map[string]interface{}{},
	}

	candidates, err := gen.Generate(context.Background(), req)
	require.NoError(t, err)
	assert.NotEmpty(t, candidates)
}
