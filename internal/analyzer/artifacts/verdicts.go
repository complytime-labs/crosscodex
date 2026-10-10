package artifacts

import (
	"context"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

// VerdictStatus is the state of one SAME_AS candidate in the verdict store.
// Only pending is open to automation; a human may later move a pair in any
// state to confirmed or rejected.
type VerdictStatus string

const (
	VerdictPending   VerdictStatus = "pending"   // queued for the panel
	VerdictConfirmed VerdictStatus = "confirmed" // the two groups denote one artifact
	VerdictRejected  VerdictStatus = "rejected"  // they do not
	VerdictUndecided VerdictStatus = "undecided" // the panel split; left for human review
	VerdictAbandoned VerdictStatus = "abandoned" // the panel failed max_attempts times
)

// Determination types rank who decided a pair, highest authority first. A
// human verdict outranks the LLM panel, which outranks the reconciler's
// token-overlap score. Automation never overwrites a human verdict.
const (
	DeterminationHuman        = "human"
	DeterminationLLMPanel     = "llm_panel"
	DeterminationTokenOverlap = tokenOverlapMethod
)

// adjudicatorName is determined_by on every verdict and edge the adjudicator writes.
const adjudicatorName = "artifact-adjudicator"

// PairKey returns the verdict-store key of the group pair low < high.
// DerivedID escapes its parts, so distinct pairs never share a key even
// though group IDs embed arbitrary artifact names.
func PairKey(low, high string) string {
	return graphdb.DerivedID("artifact-pair", low, high)
}

// MemberSample is one Artifact of a group as the panel sees it.
type MemberSample struct {
	Name      string `json:"name"`
	OwnerRole string `json:"owner_role,omitempty"`
	Frequency string `json:"frequency,omitempty"`
}

// PairContext is what the panel is shown about a pair. The reconciler
// snapshots it when it first enqueues the pair.
type PairContext struct {
	Type        string         `json:"type"`
	LowName     string         `json:"low_name"`
	HighName    string         `json:"high_name"`
	LowSamples  []MemberSample `json:"low_samples"`
	HighSamples []MemberSample `json:"high_samples"`
}

// PairCandidate is a token-overlap SAME_AS match the reconciler enqueues.
type PairCandidate struct {
	LowGroupID, HighGroupID string // LowGroupID < HighGroupID
	CandidateEdgeID         string // the token_overlap SAME_AS edge under review
	SimilarityScore         float64
	Context                 PairContext
}

// LeasedPair is a pending pair one adjudicator holds until its lease ends.
type LeasedPair struct {
	PairCandidate
	Key         string
	Attempts    int       // failed panels before this lease
	LeasedUntil time.Time // the lease this caller holds; fences RecordFailure
}

// VoteRecord is one panel vote as stored in evidence.
type VoteRecord struct {
	VoterID       string `json:"voter_id"`
	Decision      *bool  `json:"decision"` // nil: the call failed or the reply did not parse
	Confidence    string `json:"confidence,omitempty"`
	Justification string `json:"justification,omitempty"`
}

// PanelEvidence records how a panel reached its opinion.
type PanelEvidence struct {
	PromptName    string       `json:"prompt_name"`
	PromptVersion string       `json:"prompt_version"`
	Models        []string     `json:"models"`
	Votes         []VoteRecord `json:"votes"`
	ValidVotes    int          `json:"valid_votes"`
	TotalVotes    int          `json:"total_votes"`
}

// Verdict is one panel's opinion on a pair.
type Verdict struct {
	Status     VerdictStatus // confirmed, rejected or undecided
	Confidence float64       // consensus fraction
	Evidence   PanelEvidence
	DecidedAt  time.Time
}

// DecideOutcome reports what Decide did.
type DecideOutcome int

const (
	// DecideApplied: the verdict was recorded.
	DecideApplied DecideOutcome = iota + 1
	// DecideHumanOwned: a human decided the pair, so nothing changed. The
	// caller records the panel's opinion with RecordDissent.
	DecideHumanOwned
	// DecideStale: the pair is no longer pending (another run decided it),
	// so nothing changed.
	DecideStale
)

// Decision is what Decide did and, when it applied the verdict, the pair's
// candidate edge as stored at that moment.
type Decision struct {
	Outcome DecideOutcome
	// CandidateEdgeID is the edge the verdict must be projected onto. It can
	// differ from the LeasedPair's copy: Enqueue moves a pending pair to the
	// current algorithm version's edge even while an adjudicator holds its
	// lease. Empty unless Outcome is DecideApplied.
	CandidateEdgeID string
}

// Failure is one failed panel attempt.
type Failure struct {
	Err           string    // stored in last_error
	At            time.Time // stamps decided_at when Abandon is set
	NextAttemptAt time.Time // earliest time the pair may be leased again
	Abandon       bool      // move the pair to abandoned instead of retrying
	LeasedUntil   time.Time // the caller's lease (LeasedPair.LeasedUntil); a failure recorded under an expired, replaced lease is ignored
}

// Projection is a decided automated verdict not yet written to the graph.
type Projection struct {
	Key                     string
	LowGroupID, HighGroupID string
	CandidateEdgeID         string
	Status                  VerdictStatus // confirmed or rejected
	Confidence              float64
	Evidence                PanelEvidence
	DecidedAt               time.Time
}

// QueueCounts summarizes a tenant's queue for a dry run.
type QueueCounts struct {
	Pending     int // pending, not human-owned
	Due         int // pending and leasable now
	Unprojected int // decided automated confirmed/rejected verdicts not yet in the graph
}

// VerdictStore is the source of truth for SAME_AS verdicts and the
// adjudicator's work queue. PGVerdictStore is the production implementation.
//
// Rules every implementation keeps:
//   - Lease, Decide, RecordFailure, Unprojected and MarkProjected never touch
//     a row whose determination type is human.
//   - Decide applies only to pending rows.
//   - Lease never hands a pair to two callers whose leases overlap.
type VerdictStore interface {
	// Enqueue adds each candidate as a pending token_overlap row, due at now.
	// A pair that is already queued and still pending and automation-owned
	// takes the candidate's edge ID and score, so it always names the current
	// algorithm version's edge; the rest of its row (context snapshot,
	// attempts, due time, lease) is kept. A decided or human-owned pair is
	// left exactly as it is.
	Enqueue(ctx context.Context, tenantID string, candidates []PairCandidate, now time.Time) error
	// ClosedPairs returns the keys of pairs whose status is confirmed or
	// rejected, whoever decided them.
	ClosedPairs(ctx context.Context, tenantID string) (map[string]bool, error)
	// Lease takes up to n due pending pairs (next_attempt_at <= now, no
	// active lease, not human-owned) and holds each until now+leaseFor.
	Lease(ctx context.Context, tenantID string, n int, leaseFor time.Duration, now time.Time) ([]LeasedPair, error)
	// Decide records an llm_panel verdict by the adjudicator and, when it
	// applies, returns the pair's stored candidate edge ID.
	Decide(ctx context.Context, tenantID, key string, v Verdict) (Decision, error)
	// RecordDissent stores v as the automated opinion on a human-owned pair.
	// It does nothing to any other pair.
	RecordDissent(ctx context.Context, tenantID, key string, v Verdict) error
	// RecordFailure counts a failed panel on a pending, automation-owned pair
	// and releases its lease. It does nothing unless the caller still holds
	// the lease (f.LeasedUntil equals the pair's current lease), and nothing
	// to any other pair.
	RecordFailure(ctx context.Context, tenantID, key string, f Failure) error
	// Unprojected returns up to n decided automated confirmed/rejected
	// verdicts not yet projected, oldest decision first.
	Unprojected(ctx context.Context, tenantID string, n int) ([]Projection, error)
	// MarkProjected records that a verdict is in the graph. It does nothing to
	// a human-owned pair or one that is not confirmed or rejected.
	MarkProjected(ctx context.Context, tenantID, key string) error
	// Counts summarizes the queue as of now.
	Counts(ctx context.Context, tenantID string, now time.Time) (QueueCounts, error)
}
