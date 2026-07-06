package graphdb

import "time"

// RequiresEdge represents a REQUIRES relationship edge in the graph,
// created from consensus voting on prerequisite dependency detection.
// The source control requires the target control to be in place first.
type RequiresEdge struct {
	SourceID        string    // Source control ID
	TargetID        string    // Target control ID (prerequisite)
	Confidence      float64   // Consensus confidence fraction [0.0, 1.0]
	Unanimous       bool      // All votes agreed
	ValidVotes      int       // Number of successful votes
	TotalVotes      int       // Total votes (including errors)
	VoteWeight      float64   // Total weighted votes
	Models          []string  // LLM models used
	SamplesPerModel int       // Samples per model
	PromptVersion   string    // Prompt version
	AnalyzedAt      time.Time // When consensus was computed
	TenantID        string    // Tenant identifier
	JobID           string    // Analysis job identifier
}
