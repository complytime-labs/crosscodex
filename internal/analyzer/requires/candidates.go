package requires

import "context"

// CandidateProvider supplies candidate prerequisite pairs for requires analysis.
// The pipeline orchestrator implements this interface, bridging prerequisite
// detection output to the requires analyzer. The analyzer itself has no
// direct dependency on prerequisite generation implementation details.
type CandidateProvider interface {
	Candidates(ctx context.Context, tenantID, jobID string) ([]RequiresPair, error)
}
