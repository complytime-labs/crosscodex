package candidate_test

import (
	"context"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockGenerator is a test generator
type mockGenerator struct {
	name       string
	candidates []candidate.Candidate
	err        error
}

func (m *mockGenerator) Name() string {
	return m.name
}

func (m *mockGenerator) Generate(ctx context.Context, req candidate.GenerateRequest) ([]candidate.Candidate, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.candidates, nil
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	registry := candidate.NewRegistry()

	gen := &mockGenerator{name: "test-gen"}
	err := registry.Register(gen)
	require.NoError(t, err)

	retrieved, err := registry.Get("test-gen")
	require.NoError(t, err)
	assert.Equal(t, "test-gen", retrieved.Name())
}

func TestRegistry_RegisterDuplicate(t *testing.T) {
	registry := candidate.NewRegistry()

	gen1 := &mockGenerator{name: "test-gen"}
	gen2 := &mockGenerator{name: "test-gen"}

	err := registry.Register(gen1)
	require.NoError(t, err)

	err = registry.Register(gen2)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already registered")
}

func TestRegistry_GetNotFound(t *testing.T) {
	registry := candidate.NewRegistry()

	_, err := registry.Get("nonexistent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestRegistry_Generate_Union(t *testing.T) {
	registry := candidate.NewRegistry()

	// Register two generators with different candidates
	gen1 := &mockGenerator{
		name: "gen1",
		candidates: []candidate.Candidate{
			{SourceID: "S1", TargetID: "T1", Score: 0.8, Weight: 1.0, GeneratorID: "gen1"},
			{SourceID: "S1", TargetID: "T2", Score: 0.7, Weight: 1.0, GeneratorID: "gen1"},
		},
	}
	gen2 := &mockGenerator{
		name: "gen2",
		candidates: []candidate.Candidate{
			{SourceID: "S1", TargetID: "T2", Score: 0.9, Weight: 0.5, GeneratorID: "gen2"}, // Duplicate pair
			{SourceID: "S1", TargetID: "T3", Score: 0.6, Weight: 0.5, GeneratorID: "gen2"},
		},
	}

	require.NoError(t, registry.Register(gen1))
	require.NoError(t, registry.Register(gen2))

	req := candidate.GenerateRequest{
		TenantID: "tenant1",
		JobID:    "job1",
	}

	results, err := registry.Generate(context.Background(), req, candidate.StrategyUnion)
	require.NoError(t, err)

	// Union should contain all unique pairs: (S1,T1), (S1,T2), (S1,T3)
	assert.Len(t, results, 3)

	// Verify deduplication - (S1,T2) should appear only once
	pairCount := make(map[string]int)
	for _, c := range results {
		key := c.SourceID + ":" + c.TargetID
		pairCount[key]++
	}
	assert.Equal(t, 1, pairCount["S1:T1"])
	assert.Equal(t, 1, pairCount["S1:T2"])
	assert.Equal(t, 1, pairCount["S1:T3"])
}

func TestRegistry_Generate_WeightedUnion(t *testing.T) {
	registry := candidate.NewRegistry()

	// Generator 1: weight 1.0
	gen1 := &mockGenerator{
		name: "gen1",
		candidates: []candidate.Candidate{
			{SourceID: "S1", TargetID: "T1", Score: 0.8, Weight: 1.0, GeneratorID: "gen1"},
			{SourceID: "S1", TargetID: "T2", Score: 0.5, Weight: 1.0, GeneratorID: "gen1"}, // Low score
		},
	}
	// Generator 2: weight 0.5
	gen2 := &mockGenerator{
		name: "gen2",
		candidates: []candidate.Candidate{
			{SourceID: "S1", TargetID: "T1", Score: 0.9, Weight: 0.5, GeneratorID: "gen2"}, // Duplicate, high score
			{SourceID: "S1", TargetID: "T3", Score: 0.3, Weight: 0.5, GeneratorID: "gen2"}, // Very low score
		},
	}

	require.NoError(t, registry.Register(gen1))
	require.NoError(t, registry.Register(gen2))

	req := candidate.GenerateRequest{
		TenantID: "tenant1",
		JobID:    "job1",
	}

	// Use threshold 0.6 for weighted union
	results, err := registry.Generate(context.Background(), req, candidate.StrategyWeightedUnion, candidate.WithMinScore(0.6))
	require.NoError(t, err)

	// For (S1,T1): gen1(0.8*1.0) + gen2(0.9*0.5) = 0.8 + 0.45 = 1.25, normalized = 1.25/1.5 = 0.833 > 0.6 ✓
	// For (S1,T2): gen1(0.5*1.0) = 0.5, normalized = 0.5/1.0 = 0.5 < 0.6 ✗
	// For (S1,T3): gen2(0.3*0.5) = 0.15, normalized = 0.15/0.5 = 0.3 < 0.6 ✗
	assert.Len(t, results, 1)
	assert.Equal(t, "T1", results[0].TargetID)
}

func TestRegistry_Generate_Deduplication(t *testing.T) {
	registry := candidate.NewRegistry()

	gen := &mockGenerator{
		name: "gen1",
		candidates: []candidate.Candidate{
			{SourceID: "S1", TargetID: "T1", Score: 0.8, Weight: 1.0, GeneratorID: "gen1"},
			{SourceID: "S1", TargetID: "T1", Score: 0.9, Weight: 1.0, GeneratorID: "gen1"}, // Duplicate
		},
	}

	require.NoError(t, registry.Register(gen))

	req := candidate.GenerateRequest{
		TenantID: "tenant1",
		JobID:    "job1",
	}

	results, err := registry.Generate(context.Background(), req, candidate.StrategyUnion)
	require.NoError(t, err)

	// Should deduplicate
	assert.Len(t, results, 1)
}

func TestRegistry_Generate_EmptyRegistry(t *testing.T) {
	registry := candidate.NewRegistry()

	req := candidate.GenerateRequest{
		TenantID: "tenant1",
		JobID:    "job1",
	}

	results, err := registry.Generate(context.Background(), req, candidate.StrategyUnion)
	require.NoError(t, err)

	// Empty registry returns empty slice, not error
	assert.Empty(t, results)
}

func TestRegistry_Generate_GeneratorError(t *testing.T) {
	registry := candidate.NewRegistry()

	gen := &mockGenerator{
		name: "failing-gen",
		err:  assert.AnError,
	}

	require.NoError(t, registry.Register(gen))

	req := candidate.GenerateRequest{
		TenantID: "tenant1",
		JobID:    "job1",
	}

	_, err := registry.Generate(context.Background(), req, candidate.StrategyUnion)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failing-gen")
}

func TestRegistry_Generate_UnknownStrategy(t *testing.T) {
	registry := candidate.NewRegistry()

	gen := &mockGenerator{
		name: "gen1",
		candidates: []candidate.Candidate{
			{SourceID: "S1", TargetID: "T1", Score: 0.8, Weight: 1.0, GeneratorID: "gen1"},
		},
	}

	require.NoError(t, registry.Register(gen))

	req := candidate.GenerateRequest{
		TenantID: "tenant1",
		JobID:    "job1",
	}

	_, err := registry.Generate(context.Background(), req, candidate.AggregationStrategy("invalid"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown aggregation strategy")
}

func TestRegistry_All(t *testing.T) {
	registry := candidate.NewRegistry()

	gen1 := &mockGenerator{name: "gen1"}
	gen2 := &mockGenerator{name: "gen2"}

	require.NoError(t, registry.Register(gen1))
	require.NoError(t, registry.Register(gen2))

	all := registry.All()
	assert.Len(t, all, 2)

	names := make(map[string]bool)
	for _, g := range all {
		names[g.Name()] = true
	}
	assert.True(t, names["gen1"])
	assert.True(t, names["gen2"])
}

func TestRegistry_WeightedUnion_MultipleContributions(t *testing.T) {
	registry := candidate.NewRegistry()

	// Multiple generators contributing to same pair with different weights
	gen1 := &mockGenerator{
		name: "gen1",
		candidates: []candidate.Candidate{
			{SourceID: "S1", TargetID: "T1", Score: 0.6, Weight: 0.8, GeneratorID: "gen1"},
		},
	}
	gen2 := &mockGenerator{
		name: "gen2",
		candidates: []candidate.Candidate{
			{SourceID: "S1", TargetID: "T1", Score: 0.8, Weight: 0.2, GeneratorID: "gen2"},
		},
	}

	require.NoError(t, registry.Register(gen1))
	require.NoError(t, registry.Register(gen2))

	req := candidate.GenerateRequest{
		TenantID: "tenant1",
		JobID:    "job1",
	}

	// Weighted average: (0.6*0.8 + 0.8*0.2) / (0.8 + 0.2) = (0.48 + 0.16) / 1.0 = 0.64
	results, err := registry.Generate(context.Background(), req, candidate.StrategyWeightedUnion, candidate.WithMinScore(0.6))
	require.NoError(t, err)

	assert.Len(t, results, 1)
	assert.Equal(t, "T1", results[0].TargetID)
	assert.InDelta(t, 0.64, results[0].Score, 0.01)
}
