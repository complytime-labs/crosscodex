package graphdb

import (
	"strings"
	"time"
)

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

// derivedIDEscaper escapes "%" as well as "_", so a literal "%5F" in a part
// becomes "%255F" and cannot be mistaken for an escaped separator.
var derivedIDEscaper = strings.NewReplacer("%", "%25", "_", "%5F")

// DerivedID builds a graph node or edge ID from parts, such as a kind tag,
// a job ID and control IDs. Each part is percent-escaped ("%" to "%25", "_"
// to "%5F") and the escaped parts are joined with "_". Because "_" appears
// only as a separator, distinct non-empty part lists give distinct IDs, even
// when the parts are tenant-supplied and contain underscores. The empty list
// and [""] both give "", so callers always pass a kind tag as the first part.
// Parts without "_" or "%" appear unchanged, so typical IDs stay readable.
func DerivedID(parts ...string) string {
	escaped := make([]string, len(parts))
	for i, p := range parts {
		escaped[i] = derivedIDEscaper.Replace(p)
	}
	return strings.Join(escaped, "_")
}

// EdgeID returns the deterministic ID drivers write on a REQUIRES edge:
// DerivedID("requires", JobID, SourceID, TargetID). Re-materializing the same
// job's consensus for the same pair yields the same ID, so the driver rejects
// the duplicate with ErrEdgeExists; materializers treat that rejection as
// success, which is how graph writes stay idempotent. Distinct (job, source,
// target) triples always get distinct IDs.
func (r RequiresEdge) EdgeID() string {
	return DerivedID("requires", r.JobID, r.SourceID, r.TargetID)
}
