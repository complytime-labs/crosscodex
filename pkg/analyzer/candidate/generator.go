package candidate

import "context"

// Generator produces candidate pairs for analysis with provenance and scoring.
// Implementations must be safe for concurrent use.
type Generator interface {
	// Name returns the unique identifier for this generator (e.g., "semantic", "foundational").
	Name() string

	// Generate produces weighted candidate pairs from the given context.
	// Returns an empty slice (not an error) if no candidates are found.
	// It never returns a pair whose SourceID equals its TargetID: single-catalog
	// analysis passes the same controls as sources and targets, and a self-pair
	// wastes an LLM call per analyzer and can produce a self-mapping.
	Generate(ctx context.Context, req GenerateRequest) ([]Candidate, error)
}
