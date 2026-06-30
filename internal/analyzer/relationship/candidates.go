package relationship

import "context"

// CandidateProvider supplies candidate control pairs for relationship analysis.
// The pipeline orchestrator implements this interface, bridging embedding
// topKPairs() output to the relationship analyzer. The analyzer itself has no
// direct dependency on pkg/vectordb or the embedding package.
type CandidateProvider interface {
	Candidates(ctx context.Context, tenantID, jobID string) ([]CandidatePair, error)
}

// CandidatePair represents a pair of controls identified as similar by
// embedding analysis, with their similarity score from the embedding matrix.
type CandidatePair struct {
	SourceControlID string  // Source control identifier
	TargetControlID string  // Target control identifier
	SimilarityScore float32 // [0, 100] from embedding similarity matrix
}
