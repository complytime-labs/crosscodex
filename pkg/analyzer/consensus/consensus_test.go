package consensus_test

import (
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/consensus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputer_SimpleMajority(t *testing.T) {
	computer := consensus.New()

	yes, no := true, false
	votes := []consensus.Vote{
		{VoterID: "m1", Decision: &yes, Weight: 1.0},
		{VoterID: "m2", Decision: &yes, Weight: 1.0},
		{VoterID: "m3", Decision: &no, Weight: 1.0},
	}

	result, err := computer.Compute(votes)
	require.NoError(t, err)

	assert.True(t, result.Decision)
	assert.Equal(t, 2.0/3.0, result.ConfidenceFraction)
	assert.False(t, result.Unanimous)
	assert.Equal(t, 3, result.ValidVoteCount)
	assert.Equal(t, 3, result.TotalVoteCount)
}

func TestComputer_Unanimous(t *testing.T) {
	computer := consensus.New()

	yes := true
	votes := []consensus.Vote{
		{VoterID: "m1", Decision: &yes, Weight: 1.0},
		{VoterID: "m2", Decision: &yes, Weight: 1.0},
		{VoterID: "m3", Decision: &yes, Weight: 1.0},
	}

	result, err := computer.Compute(votes)
	require.NoError(t, err)

	assert.True(t, result.Decision)
	assert.Equal(t, 1.0, result.ConfidenceFraction)
	assert.True(t, result.Unanimous)
}

func TestComputer_TieBreaker(t *testing.T) {
	// Even votes default to false (conservative)
	computer := consensus.New()

	yes, no := true, false
	votes := []consensus.Vote{
		{VoterID: "m1", Decision: &yes, Weight: 1.0},
		{VoterID: "m2", Decision: &no, Weight: 1.0},
	}

	result, err := computer.Compute(votes)
	require.NoError(t, err)

	assert.False(t, result.Decision) // Tie defaults to false
}

func TestComputer_WeightedVotes(t *testing.T) {
	computer := consensus.New()

	yes, no := true, false
	votes := []consensus.Vote{
		{VoterID: "m1", Decision: &yes, Weight: 2.0}, // Weighted model
		{VoterID: "m2", Decision: &no, Weight: 1.0},
		{VoterID: "m3", Decision: &no, Weight: 1.0},
	}

	result, err := computer.Compute(votes)
	require.NoError(t, err)

	// yes=2.0, no=2.0 -> tie -> false
	assert.False(t, result.Decision)
	assert.Equal(t, 2.0/4.0, result.ConfidenceFraction)
}

func TestComputer_MinValidVotes(t *testing.T) {
	computer := consensus.New(consensus.WithMinValidVotes(5))

	yes := true
	votes := []consensus.Vote{
		{VoterID: "m1", Decision: &yes, Weight: 1.0},
		{VoterID: "m2", Decision: &yes, Weight: 1.0},
		{VoterID: "m3", Decision: nil}, // Error
	}

	_, err := computer.Compute(votes)
	require.Error(t, err)

	var insufficientErr *consensus.ErrInsufficientVotes
	assert.ErrorAs(t, err, &insufficientErr)
	assert.Equal(t, 2, insufficientErr.ValidCount)
	assert.Equal(t, 5, insufficientErr.RequiredMin)
}

func TestComputer_MaxErrorRate(t *testing.T) {
	computer := consensus.New(consensus.WithMaxErrorRate(0.25)) // Allow 25% errors

	yes := true
	votes := []consensus.Vote{
		{VoterID: "m1", Decision: &yes},
		{VoterID: "m2", Decision: &yes},
		{VoterID: "m3", Decision: nil}, // Error
		{VoterID: "m4", Decision: nil}, // Error (50% error rate)
	}

	_, err := computer.Compute(votes)
	require.Error(t, err) // 50% > 25% threshold
}

func TestComputer_DefaultWeight(t *testing.T) {
	// Votes with Weight=0 should use 1.0
	computer := consensus.New()

	yes, no := true, false
	votes := []consensus.Vote{
		{VoterID: "m1", Decision: &yes, Weight: 0}, // Should be 1.0
		{VoterID: "m2", Decision: &no, Weight: 0},  // Should be 1.0
	}

	result, err := computer.Compute(votes)
	require.NoError(t, err)

	assert.Equal(t, 2.0, result.TotalWeight) // Both counted as 1.0
}
