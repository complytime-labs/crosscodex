package critique

import (
	"context"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
)

// Kinds of proposed verdict: the analyzer whose panel proposed it.
const (
	KindRelationship = results.RelationshipAnalyzer
	KindRequires     = results.RequiresAnalyzer
)

// ProposedVerdict is one verdict the critique panel reviews. For Kind
// KindRelationship, Verdict is the consensus relationship type (never
// NO_RELATIONSHIP). For KindRequires, Verdict states that the source
// requires the target. Justifications are the proposing panel's reasons.
// They are model output, so they reach the prompt only through the prompt
// hardening.
type ProposedVerdict struct {
	Kind            string
	SourceControlID string
	TargetControlID string
	Verdict         string
	Justifications  []string
}

// VerdictProvider supplies the verdicts a job's critique stage reviews. The
// pipeline implements it from the job's stored relationship and requires
// results. It returns only accepted verdicts: related relationship pairs, and
// requires pairs that cleared their threshold.
type VerdictProvider interface {
	Verdicts(ctx context.Context, tenantID, jobID string) ([]ProposedVerdict, error)
}
