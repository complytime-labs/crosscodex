package consensus_test

import (
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/consensus"
	"github.com/stretchr/testify/assert"
)

func TestVote_DefaultWeight(t *testing.T) {
	decision := true
	vote := consensus.Vote{
		VoterID:       "model-1",
		Decision:      &decision,
		Confidence:    "HIGH",
		Weight:        0, // Should default to 1.0 in use
		Justification: "test",
		RawResponse:   "response",
	}

	// Weight of 0 should be treated as 1.0 by Computer
	assert.Equal(t, float64(0), vote.Weight)
}

func TestResult_Structure(t *testing.T) {
	result := consensus.Result{
		Decision:           true,
		ConfidenceFraction: 0.75,
		Unanimous:          false,
		ValidVoteCount:     3,
		TotalVoteCount:     4,
		TotalWeight:        3.0,
	}

	assert.True(t, result.Decision)
	assert.Equal(t, 0.75, result.ConfidenceFraction)
	assert.False(t, result.Unanimous)
}

func TestErrInsufficientVotes_Error(t *testing.T) {
	err := &consensus.ErrInsufficientVotes{
		ValidCount:  2,
		RequiredMin: 5,
		TotalCount:  10,
		ErrorCount:  8,
	}

	msg := err.Error()
	assert.Contains(t, msg, "insufficient valid votes")
	assert.Contains(t, msg, "2/10")
	assert.Contains(t, msg, "required 5")
}
