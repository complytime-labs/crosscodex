package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/complytime-labs/crosscodex/pkg/db"
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

// pairRefLen is the length of a PairRef in hex characters (64 bits).
const pairRefLen = 16

var (
	// ErrPairNotFound: no pair of the tenant has the given ref.
	ErrPairNotFound = errors.New("no pair of this tenant has this ref")
	// ErrPairRefAmbiguous: more than one pair of the tenant shares the ref.
	ErrPairRefAmbiguous = errors.New("more than one pair has this ref")
)

// PairRef returns the short, opaque handle operators use for a pair: the
// first 16 hex characters of sha256(key). Pair keys embed untrusted artifact
// names, so they are unsafe to print or paste; PGVerdictStore computes the
// same value in SQL to resolve a ref.
func PairRef(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:pairRefLen]
}

// ValidatePairRef reports whether ref has PairRef's shape.
func ValidatePairRef(ref string) error {
	if len(ref) != pairRefLen || strings.Trim(ref, "0123456789abcdef") != "" {
		return fmt.Errorf("pair ref %q is not %d lowercase hex characters; copy the PAIR column from the review listing", ref, pairRefLen)
	}
	return nil
}

// unprintable returns the first control, format or line/paragraph separator
// character in s, which ValidateSearchName refuses.
func unprintable(s string) (rune, bool) {
	for _, r := range s {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return r, true
		}
	}
	return 0, false
}

// MaxSearchNameBytes bounds SearchPairs' name text.
const MaxSearchNameBytes = 200

// ValidateSearchName checks SearchPairs' name text, which comes from an
// operator. It is matched literally, so wildcards need no check here.
func ValidateSearchName(name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return errors.New("search name is empty: pass part of the LOW or HIGH name of the pair to find")
	case len(name) > MaxSearchNameBytes:
		return fmt.Errorf("search name is %d bytes: search with at most %d bytes of the name", len(name), MaxSearchNameBytes)
	case !utf8.ValidString(name):
		return errors.New("search name is not valid UTF-8: pass plain text")
	}
	if r, bad := unprintable(name); bad {
		return fmt.Errorf("search name contains control, format or line-separator characters (%U): pass plain text; listings print names without such characters, so search with the visible part", r)
	}
	return nil
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

// HumanVerdict is a reviewer's decision on a pair.
type HumanVerdict struct {
	Status    VerdictStatus // confirmed or rejected
	Reviewer  string        // stored as determined_by; must pass db.ValidateReviewerID
	DecidedAt time.Time
	// Expect is the PairDetail Expect the reviewer decided from; it must
	// pass ValidateExpect.
	Expect string
}

// HumanOutcome reports what DecideHuman did.
type HumanOutcome int

const (
	// HumanApplied: the verdict was recorded and queued for projection.
	HumanApplied HumanOutcome = iota + 1
	// HumanUnchanged: a human had already recorded this status; nothing changed.
	HumanUnchanged
	// HumanReaffirmed: a human had already recorded this status and a panel
	// contradicted it afterwards; the verdict was recorded again under the
	// new reviewer and decision time, which takes the pair off the review
	// queue, and queued for projection.
	HumanReaffirmed
)

// HumanDecision is DecideHuman's result.
type HumanDecision struct {
	Outcome HumanOutcome
	Key     string // the pair the ref resolved to
	// ClaimedUntil is the pair's projection claim when it is still live at
	// the decision time, else zero. A live claim means a projection pass is
	// writing the pair's previous verdict; the new verdict reaches the graph
	// when that pass finishes or once the claim expires.
	ClaimedUntil time.Time
}

// ReviewItem is one pair as ReviewQueue and SearchPairs list it. Context
// values are untrusted; pass them through CleanDisplay before printing.
type ReviewItem struct {
	Ref               string
	Key               string
	Status            VerdictStatus
	DeterminationType string
	SimilarityScore   float64
	Context           PairContext
	DissentStatus     VerdictStatus // "" when there is no dissent
	// Votes is the current llm_panel verdict's vote split; nil for other
	// determinations. Missing or undecodable llm_panel evidence leaves it nil
	// and sets VotesUnreadable instead.
	Votes           *VoteSplit
	VotesUnreadable bool
}

// validateHumanVerdict checks DecideHuman's input before any row is read.
func validateHumanVerdict(ref string, h HumanVerdict) error {
	if h.Status != VerdictConfirmed && h.Status != VerdictRejected {
		return fmt.Errorf("verdict %q is not a human verdict: use confirmed or rejected", h.Status)
	}
	if h.DecidedAt.IsZero() {
		return errors.New("human verdict decision time is not set: pass the time the reviewer decided")
	}
	if err := ValidatePairRef(ref); err != nil {
		return err
	}
	if err := db.ValidateReviewerID(h.Reviewer); err != nil {
		return err
	}
	return ValidateExpect(h.Expect)
}

// Projection is a decided verdict not yet written to the graph.
type Projection struct {
	Key                     string
	LowGroupID, HighGroupID string
	CandidateEdgeID         string
	Status                  VerdictStatus // confirmed or rejected
	Confidence              float64
	Evidence                PanelEvidence // set only for an llm_panel verdict
	DecidedAt               time.Time
	DeterminationType       string // llm_panel or human
	DeterminedBy            string // the reviewer, for a human verdict
}

// ClaimOutcome reports what ClaimProjection did.
type ClaimOutcome int

const (
	// ClaimAcquired: the caller holds the pair's projection claim and may
	// write the verdict to the graph, then release the claim with
	// MarkProjected.
	ClaimAcquired ClaimOutcome = iota + 1
	// ClaimReplaced: the pair no longer holds the verdict; a newer one
	// replaced it and is queued. Write nothing.
	ClaimReplaced
	// ClaimBusy: the pair holds the verdict, but another live claim holds it
	// or it is already projected. Write nothing; the claimant finishes it.
	ClaimBusy
)

// QueueCounts summarizes a tenant's queue for a dry run.
type QueueCounts struct {
	Pending     int // pending, not human-owned
	Due         int // pending and leasable now
	Unprojected int // decided confirmed/rejected verdicts not yet in the graph, including ones a projection pass has claimed
}

// VerdictStore is the source of truth for SAME_AS verdicts and the
// adjudicator's work queue. PGVerdictStore is the production implementation.
//
// Rules every implementation keeps:
//   - Enqueue, Lease, Decide and RecordFailure never modify a row whose
//     determination type is human.
//   - Decide applies only to pending rows.
//   - Lease never hands a pair to two callers whose leases overlap.
//   - Projection is claim, write, release: ClaimProjection grants a decided,
//     unprojected row to one caller until its claim expires, Unprojected
//     hides rows under a live claim, and MarkProjected releases only the
//     caller's own claim. While a claim is live, no other caller writes the
//     row's verdict to the graph, so a newer verdict is projected after the
//     claimant's graph writes, never before them.
//   - MarkProjected is a fence: it marks a row projected only if the row
//     still holds the verdict that was projected, and otherwise leaves it
//     unprojected so the newer verdict is projected next.
//   - Only ClaimProjection, MarkProjected and ReleaseClaim change a claim;
//     DecideHuman, Decide and ResetProjection leave it as it is.
//   - Timestamps are stored at microsecond precision; equality on
//     decided_at compares at that precision.
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
	// DecideHuman records h on the tenant's pair with the given ref. Any
	// state may be decided. A pair whose current verdict is already a human
	// verdict with h.Status is left unchanged (HumanUnchanged), unless a
	// panel contradicted that verdict afterwards (the condition ReviewQueue
	// lists it for): then h is recorded again as a re-affirmation
	// (HumanReaffirmed), which takes h.Reviewer and h.DecidedAt, keeps the
	// dissent and so takes the pair off the review queue. Otherwise the
	// row becomes human-owned with confidence 1, its lease and last error are
	// cleared, it is queued for projection, and an llm_panel verdict it held
	// moves to dissent. Its projection claim is kept, and reported in
	// ClaimedUntil when it is live at h.DecidedAt. Returns ErrPairNotFound or
	// ErrPairRefAmbiguous (wrapped) when ref resolves to no pair or to
	// several.
	//
	// h.Expect must be the Expect PairDetail returned for the pair. When the
	// pair no longer holds that state, DecideHuman changes nothing and returns
	// a *PairChangedError (errors.Is ErrPairChanged), unless the pair already
	// holds an undisputed human verdict with h.Status: that is HumanUnchanged,
	// whatever the token, so re-running a decide whose projection failed
	// still finishes it.
	DecideHuman(ctx context.Context, tenantID, ref string, h HumanVerdict) (HumanDecision, error)
	// ReviewQueue returns up to n pairs that need a human, oldest created
	// first: undecided and abandoned pairs, and human verdicts that a panel
	// contradicted afterwards (a confirmed or rejected dissent, different from
	// the human status, decided after the human verdict). The panel verdict a
	// human overrode is dissent too, but it predates the human verdict, so the
	// override alone does not list a pair.
	ReviewQueue(ctx context.Context, tenantID string, n int) ([]ReviewItem, error)
	// SearchPairs returns up to n of the tenant's pairs, in any status and
	// whoever decided them, whose Context.LowName or Context.HighName contains
	// name case-insensitively, oldest created first. %, _ and \ in name match
	// only themselves. name must pass ValidateSearchName. DissentStatus is the
	// stored dissent's status, as in ReviewQueue, so a human override of a
	// panel verdict shows the overridden status.
	SearchPairs(ctx context.Context, tenantID, name string, n int) ([]ReviewItem, error)
	// PairDetail returns the tenant's pair with the given ref and up to
	// relatedLimit other pairs that share one of its artifact groups (newest
	// decision first, undecided last, then by key), with how many there are.
	// It changes nothing. Evidence that cannot be decoded is reported in
	// EvidenceErr (and Votes is then nil, VotesUnreadable true), not as an
	// error, so the caller still gets Expect. relatedLimit must be positive.
	// Returns ErrPairNotFound or ErrPairRefAmbiguous (wrapped) as DecideHuman
	// does.
	PairDetail(ctx context.Context, tenantID, ref string, relatedLimit int) (PairDetail, error)
	// Unprojected returns up to n decided llm_panel or human confirmed/rejected
	// verdicts not yet projected and not under a live projection claim (one
	// that ends after now), oldest decision first.
	Unprojected(ctx context.Context, tenantID string, n int, now time.Time) ([]Projection, error)
	// ClaimProjection claims pair p.Key for projecting p until until, if the
	// pair still holds p (p.DeterminationType, p.Status and p.DecidedAt), is
	// not projected, and has no live claim (none, or one that ends at or
	// before now). Otherwise it reports ClaimReplaced when the pair no longer
	// holds p, or ClaimBusy. until must be after now. Returns an error for a
	// pair that is not queued. A claim covers [now, until): at until it has
	// ended, as an edge has at its valid_to.
	ClaimProjection(ctx context.Context, tenantID string, p Projection, until, now time.Time) (ClaimOutcome, error)
	// MarkProjected releases the caller's claim on pair p.Key, the one
	// ClaimProjection granted until claimedUntil, and sets graph_applied to
	// whether the pair still holds the verdict p describes; it reports that.
	// false with the claim released means a newer verdict replaced p while it
	// was projected; the pair stays queued. A pair whose claim is not
	// claimedUntil (it expired and another caller claimed or released it) is
	// left exactly as it is and reported false.
	MarkProjected(ctx context.Context, tenantID string, p Projection, claimedUntil time.Time) (bool, error)
	// ReleaseClaim clears the projection claim on pair key if it is the one
	// ClaimProjection granted until claimedUntil, leaving graph_applied as it
	// is, so the pair can be claimed again at once. Use it when a projection
	// fails before writing anything to the graph. A pair under another claim,
	// or none, or not queued, is left alone.
	ReleaseClaim(ctx context.Context, tenantID, key string, claimedUntil time.Time) error
	// ResetProjection marks every projected llm_panel or human confirmed or
	// rejected verdict of the tenant unprojected, so the next Run writes it to
	// the graph again, and returns how many it marked. Use it after the
	// tenant's graph was rebuilt. A claimed row is never projected (claims are
	// taken only on unprojected rows and released when marking), so the reset
	// never meets a claim and leaves claims alone.
	ResetProjection(ctx context.Context, tenantID string) (int, error)
	// Counts summarizes the queue as of now.
	Counts(ctx context.Context, tenantID string, now time.Time) (QueueCounts, error)
}
