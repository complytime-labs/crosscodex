package requires

// RequiresPair represents a pair of controls identified as having a prerequisite
// relationship, with aggregated score from candidate generators and provenance
// information tracking which generators contributed.
type RequiresPair struct {
	SourceControlID string                // Source control identifier
	TargetControlID string                // Target control identifier
	AggregateScore  float64               // Aggregated score from providers
	Provenance      []CandidateProvenance // Score sources and their contributions
}

// CandidateProvenance tracks which generator contributed to a pair's score,
// its individual score, weight in the aggregation, and metadata about how
// the score was computed.
type CandidateProvenance struct {
	GeneratorName string            // Name of the provider that generated this score
	Score         float64           // Score from this provider [0, 1]
	Weight        float64           // Weight in aggregation [0, 1]
	Metadata      map[string]string // Additional generation context (model, method, etc.)
}
