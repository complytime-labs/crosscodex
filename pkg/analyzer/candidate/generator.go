package candidate

import "context"

// Generator produces candidate pairs for analysis with provenance and scoring.
// Implementations must be safe for concurrent use.
type Generator interface {
	// Name returns the unique identifier for this generator (e.g., "semantic", "foundational").
	Name() string

	// Generate produces weighted candidate pairs from the given context.
	// Returns an empty slice (not an error) if no candidates are found.
	Generate(ctx context.Context, req GenerateRequest) ([]Candidate, error)
}
