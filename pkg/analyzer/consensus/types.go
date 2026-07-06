package consensus

import "fmt"

// Vote represents a single model's vote on a binary decision.
type Vote struct {
	VoterID       string  // Model identifier (e.g., "llama3.2:3b__s0")
	Decision      *bool   // true/false/nil (nil = error/no vote)
	Confidence    string  // "HIGH", "MEDIUM", "LOW"
	Weight        float64 // Vote weight (default 1.0 if 0)
	Justification string  // LLM's reasoning
	RawResponse   string  // Full LLM output for debugging
}

// Result contains the consensus decision and metadata.
type Result struct {
	Decision           bool    // Final decision (weighted majority)
	ConfidenceFraction float64 // [0.0, 1.0] - weighted_majority / total_weight
	Unanimous          bool    // All valid votes agree
	ValidVoteCount     int     // Number of non-nil votes
	TotalVoteCount     int     // Total votes (including errors)
	TotalWeight        float64 // Sum of weights from valid votes
}

// ErrInsufficientVotes is returned when valid vote count is below threshold.
type ErrInsufficientVotes struct {
	ValidCount  int
	RequiredMin int
	TotalCount  int
	ErrorCount  int
}

func (e *ErrInsufficientVotes) Error() string {
	return fmt.Sprintf("consensus: insufficient valid votes (%d/%d, required %d minimum, %d errors)",
		e.ValidCount, e.TotalCount, e.RequiredMin, e.ErrorCount)
}
