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

func TestLevelGenerator_Name(t *testing.T) {
	gen := builtin.NewLevelGenerator()
	assert.Equal(t, "level", gen.Name())
}

func TestLevelGenerator_Generate_OperationalRequiresTactical(t *testing.T) {
	gen := builtin.NewLevelGenerator()

	sources := map[string]*candidate.ControlData{
		"OP-1": {
			ControlID: "OP-1",
			Text:      "Operational control 1",
			Level:     "Operational",
		},
		"OP-2": {
			ControlID: "OP-2",
			Text:      "Operational control 2",
			Level:     "Operational",
		},
	}

	targets := map[string]*candidate.ControlData{
		"TAC-1": {
			ControlID: "TAC-1",
			Text:      "Tactical control 1",
			Level:     "Tactical",
		},
		"STRAT-1": {
			ControlID: "STRAT-1",
			Text:      "Strategic control 1",
			Level:     "Strategic",
		},
		"OP-3": {
			ControlID: "OP-3",
			Text:      "Operational control 3",
			Level:     "Operational",
		},
	}

	req := candidate.GenerateRequest{
		TenantID:       "test-tenant",
		JobID:          "test-job",
		SourceControls: sources,
		TargetControls: targets,
		Parameters: map[string]interface{}{
			"weight": 0.7,
		},
	}

	candidates, err := gen.Generate(context.Background(), req)
	require.NoError(t, err)

	// Operational sources (OP-1, OP-2) can require Tactical (TAC-1) and Strategic (STRAT-1)
	// Should NOT pair with other Operational (OP-3)
	// 2 sources × 2 higher-level targets = 4 candidates
	require.Len(t, candidates, 4)

	hasPair := func(sourceID, targetID string) bool {
		for _, c := range candidates {
			if c.SourceID == sourceID && c.TargetID == targetID {
				return true
			}
		}
		return false
	}

	// Operational can require Tactical and Strategic
	assert.True(t, hasPair("OP-1", "TAC-1"))
	assert.True(t, hasPair("OP-1", "STRAT-1"))
	assert.True(t, hasPair("OP-2", "TAC-1"))
	assert.True(t, hasPair("OP-2", "STRAT-1"))

	// Operational should NOT require other Operational
	assert.False(t, hasPair("OP-1", "OP-3"))
	assert.False(t, hasPair("OP-2", "OP-3"))

	// Verify scoring and metadata
	for _, c := range candidates {
		assert.Equal(t, 1.0, c.Score, "Level-based match should have score 1.0")
		assert.Equal(t, 0.7, c.Weight)
		assert.Equal(t, "level", c.GeneratorID)
		assert.Contains(t, c.Metadata, "source_level")
		assert.Contains(t, c.Metadata, "target_level")
	}
}

func TestLevelGenerator_Generate_TacticalRequiresStrategic(t *testing.T) {
	gen := builtin.NewLevelGenerator()

	sources := map[string]*candidate.ControlData{
		"TAC-1": {
			ControlID: "TAC-1",
			Text:      "Tactical control",
			Level:     "Tactical",
		},
	}

	targets := map[string]*candidate.ControlData{
		"STRAT-1": {
			ControlID: "STRAT-1",
			Text:      "Strategic control",
			Level:     "Strategic",
		},
		"TAC-2": {
			ControlID: "TAC-2",
			Text:      "Another tactical control",
			Level:     "Tactical",
		},
		"OP-1": {
			ControlID: "OP-1",
			Text:      "Operational control",
			Level:     "Operational",
		},
	}

	req := candidate.GenerateRequest{
		TenantID:       "test-tenant",
		JobID:          "test-job",
		SourceControls: sources,
		TargetControls: targets,
		Parameters: map[string]interface{}{
			"weight": 1.0,
		},
	}

	candidates, err := gen.Generate(context.Background(), req)
	require.NoError(t, err)

	// Tactical can only require Strategic
	require.Len(t, candidates, 1)
	assert.Equal(t, "TAC-1", candidates[0].SourceID)
	assert.Equal(t, "STRAT-1", candidates[0].TargetID)
}

func TestLevelGenerator_Generate_StrategicRequiresNothing(t *testing.T) {
	gen := builtin.NewLevelGenerator()

	sources := map[string]*candidate.ControlData{
		"STRAT-1": {
			ControlID: "STRAT-1",
			Text:      "Strategic control",
			Level:     "Strategic",
		},
	}

	targets := map[string]*candidate.ControlData{
		"STRAT-2": {
			ControlID: "STRAT-2",
			Text:      "Another strategic",
			Level:     "Strategic",
		},
		"TAC-1": {
			ControlID: "TAC-1",
			Text:      "Tactical control",
			Level:     "Tactical",
		},
		"OP-1": {
			ControlID: "OP-1",
			Text:      "Operational control",
			Level:     "Operational",
		},
	}

	req := candidate.GenerateRequest{
		TenantID:       "test-tenant",
		JobID:          "test-job",
		SourceControls: sources,
		TargetControls: targets,
		Parameters: map[string]interface{}{
			"weight": 1.0,
		},
	}

	candidates, err := gen.Generate(context.Background(), req)
	require.NoError(t, err)

	// Strategic is top-level, cannot require anything higher
	assert.Empty(t, candidates)
}

func TestLevelGenerator_Generate_UnknownLevel(t *testing.T) {
	gen := builtin.NewLevelGenerator()

	sources := map[string]*candidate.ControlData{
		"UNK-1": {
			ControlID: "UNK-1",
			Text:      "Unknown level control",
			Level:     "Unknown",
		},
	}

	targets := map[string]*candidate.ControlData{
		"STRAT-1": {
			ControlID: "STRAT-1",
			Text:      "Strategic control",
			Level:     "Strategic",
		},
	}

	req := candidate.GenerateRequest{
		TenantID:       "test-tenant",
		JobID:          "test-job",
		SourceControls: sources,
		TargetControls: targets,
		Parameters: map[string]interface{}{
			"weight": 1.0,
		},
	}

	candidates, err := gen.Generate(context.Background(), req)
	require.NoError(t, err)

	// Unknown levels should be skipped
	assert.Empty(t, candidates)
}

func TestLevelGenerator_Generate_EmptyLevel(t *testing.T) {
	gen := builtin.NewLevelGenerator()

	sources := map[string]*candidate.ControlData{
		"EMPTY-1": {
			ControlID: "EMPTY-1",
			Text:      "Control with empty level",
			Level:     "",
		},
	}

	targets := map[string]*candidate.ControlData{
		"STRAT-1": {
			ControlID: "STRAT-1",
			Text:      "Strategic control",
			Level:     "Strategic",
		},
	}

	req := candidate.GenerateRequest{
		TenantID:       "test-tenant",
		JobID:          "test-job",
		SourceControls: sources,
		TargetControls: targets,
		Parameters: map[string]interface{}{
			"weight": 1.0,
		},
	}

	candidates, err := gen.Generate(context.Background(), req)
	require.NoError(t, err)

	// Empty levels should be skipped
	assert.Empty(t, candidates)
}

func TestLevelGenerator_Generate_MixedLevels(t *testing.T) {
	gen := builtin.NewLevelGenerator()

	sources := map[string]*candidate.ControlData{
		"OP-1":    {ControlID: "OP-1", Level: "Operational"},
		"TAC-1":   {ControlID: "TAC-1", Level: "Tactical"},
		"STRAT-1": {ControlID: "STRAT-1", Level: "Strategic"},
	}

	targets := map[string]*candidate.ControlData{
		"OP-2":    {ControlID: "OP-2", Level: "Operational"},
		"TAC-2":   {ControlID: "TAC-2", Level: "Tactical"},
		"STRAT-2": {ControlID: "STRAT-2", Level: "Strategic"},
	}

	req := candidate.GenerateRequest{
		TenantID:       "test-tenant",
		JobID:          "test-job",
		SourceControls: sources,
		TargetControls: targets,
		Parameters: map[string]interface{}{
			"weight": 1.0,
		},
	}

	candidates, err := gen.Generate(context.Background(), req)
	require.NoError(t, err)

	// Expected pairs:
	// OP-1 -> TAC-2, STRAT-2 (2 pairs)
	// TAC-1 -> STRAT-2 (1 pair)
	// STRAT-1 -> nothing (0 pairs)
	// Total: 3 pairs
	require.Len(t, candidates, 3)

	hasPair := func(sourceID, targetID string) bool {
		for _, c := range candidates {
			if c.SourceID == sourceID && c.TargetID == targetID {
				return true
			}
		}
		return false
	}

	assert.True(t, hasPair("OP-1", "TAC-2"))
	assert.True(t, hasPair("OP-1", "STRAT-2"))
	assert.True(t, hasPair("TAC-1", "STRAT-2"))

	// Should NOT have same-level or downward pairs
	assert.False(t, hasPair("OP-1", "OP-2"))
	assert.False(t, hasPair("TAC-1", "TAC-2"))
	assert.False(t, hasPair("TAC-1", "OP-2"))
}

func TestLevelGenerator_WithTelemetry(t *testing.T) {
	// Create noop tracer for testing
	tracer := noop.NewTracerProvider().Tracer("test")
	gen := builtin.NewLevelGenerator(builtin.WithLevelTelemetry(tracer))

	sources := map[string]*candidate.ControlData{
		"OP-1": {ControlID: "OP-1", Text: "Operational control", Level: "Operational"},
	}

	targets := map[string]*candidate.ControlData{
		"ST-1": {ControlID: "ST-1", Text: "Strategic control", Level: "Strategic"},
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
	assert.Len(t, candidates, 1)
}
