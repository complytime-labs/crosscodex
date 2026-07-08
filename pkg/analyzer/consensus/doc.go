// Package consensus provides shared voting and consensus computation for analyzers.
//
// It implements weighted majority voting with configurable thresholds and error
// handling. Extracted from the relationship analyzer to support reuse across
// requires, relationship, and future analyzers that need LLM panel voting.
//
// Usage:
//
//	computer := consensus.New(
//		consensus.WithThreshold(0.6),
//		consensus.WithMinValidVotes(7),
//	)
//	result, err := computer.Compute(votes)
//	if err != nil {
//		// Handle insufficient votes
//	}
//	// Use result.Decision, result.ConfidenceFraction
package consensus
