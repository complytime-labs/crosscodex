package feedback

import (
	"context"
	"fmt"
)

// Store is the authoritative, tenant-scoped record of mapping votes,
// determinations and reviews. Every method needs a tenant in ctx equal to
// tenantID, and is refused otherwise. Every write is all or nothing. Lists
// are sorted byte-wise.
type Store interface {
	// JobMappings returns the synthesized mappings of jobID, viable or not,
	// sorted by (source, target). The job must be the tenant's (else
	// ErrJobNotFound) and completed (else *JobNotCompletedError). The read is
	// tenant-wide, whatever user ctx carries: reviewers review jobs other
	// users created, callers are gated by the reviewer and admin roles, and
	// the job's mappings are tenant-visible in the graph already. The same
	// holds for the mapping checks of RecordVote and Determine.
	JobMappings(ctx context.Context, tenantID, jobID string) ([]Mapping, error)
	// RecordVote appends v, which must pass Vote.Validate and name a mapping
	// of a completed job of the tenant (else ErrJobNotFound,
	// *JobNotCompletedError or ErrMappingNotFound). A vote ID already stored
	// is refused.
	RecordVote(ctx context.Context, tenantID string, v Vote) error
	// LatestVotes returns each voter's latest vote on each mapping of jobID
	// (latest CreatedAt, then greatest ID), sorted by (mapping ID, voter).
	LatestVotes(ctx context.Context, tenantID, jobID string) ([]Vote, error)
	// Review returns the mapping's review, or false when no determination
	// was ever recorded for it.
	Review(ctx context.Context, tenantID, mappingID string) (Review, bool, error)
	// Determine records a reviewer's determination d and sets the mapping's
	// review to next with GraphApplied false. d.DecidedBy must be an active
	// registered reviewer of the tenant (else cleaning.ErrReviewerNotRegistered
	// or cleaning.ErrReviewerDeleted); d must pass Determination.Validate, be
	// valid or invalid (stale is MarkStale's) and name a mapping of a
	// completed job of the tenant (else ErrJobNotFound,
	// *JobNotCompletedError or ErrMappingNotFound). next must be d's review:
	// the same mapping, job and pair, Status d.Outcome, DeterminationID d.ID
	// and DecidedAt d.DecidedAt. Under a row lock, the review's current
	// determination ID must be expectPrev, "" meaning no review (else
	// ErrReviewChanged). A determination ID already stored is refused, and
	// so are next.EdgeIDs over the stored bound (ErrEdgeIDsTooLarge), as in
	// every write of edge IDs.
	Determine(ctx context.Context, tenantID string, d Determination, expectPrev string, next Review) error
	// MarkStale records re-projection's determination d and moves the
	// mapping's review to ReviewStale with d's ID and time, keeping its
	// EdgeIDs, with GraphApplied true: the graph holds nothing left to
	// change. d must pass Determination.Validate and be ReviewStale by
	// DecidedByReproject; no reviewer check applies. The review must exist
	// (else ErrReviewNotFound) and, under a row lock, have the determination
	// ID expectPrev (else ErrReviewChanged).
	MarkStale(ctx context.Context, tenantID string, d Determination, expectPrev string) error
	// MarkReprojected replaces a projected review's EdgeIDs with edgeIDs and
	// changes nothing else, DecidedAt and the updated_at column included: the
	// review's state and its latest change are unchanged, only the graph's
	// edge IDs for it are, and mapping_cleaning_states times a review by
	// decided_at, never updated_at. Under a row lock, the review
	// must exist (else ErrReviewNotFound), have the determination ID
	// expectDetermination (else ErrReviewChanged) and GraphApplied true
	// (else ErrUnprojected), so a determination recorded since re-projection
	// read the review is never overwritten.
	MarkReprojected(ctx context.Context, tenantID, mappingID, expectDetermination string, edgeIDs []string) error
	// Unprojected returns the reviews whose graph changes are not applied,
	// sorted by mapping ID.
	Unprojected(ctx context.Context, tenantID string) ([]Review, error)
	// Reviews returns every review of the tenant, sorted by mapping ID.
	Reviews(ctx context.Context, tenantID string) ([]Review, error)
	// MarkProjected records that the graph changes of determination
	// expectDetermination are applied and replaces the review's EdgeIDs with
	// edgeIDs. Under a row lock, the review must exist (else
	// ErrReviewNotFound) and still have that determination (else
	// ErrReviewChanged), so a projector that read an earlier determination
	// never marks a later one applied before its own changes are written.
	MarkProjected(ctx context.Context, tenantID, mappingID, expectDetermination string, edgeIDs []string) error
}

// checkDetermine refuses, for Determine before any read, a determination d
// that is not a reviewer's valid one or a next review that is not d's.
func checkDetermine(d Determination, next Review) error {
	if d.DecidedBy == DecidedByReproject || d.Outcome == ReviewStale {
		return fmt.Errorf("determine mapping %s: stale determinations are recorded only by MarkStale, during re-projection; "+
			"determine valid or invalid as a registered reviewer; nothing was recorded", d.MappingID)
	}
	if err := d.Validate(); err != nil {
		return err
	}
	if next.MappingID != d.MappingID || next.JobID != d.JobID || next.SourceID != d.SourceID || next.TargetID != d.TargetID ||
		next.Status != d.Outcome || next.DeterminationID != d.ID || !next.DecidedAt.Equal(d.DecidedAt) {
		return fmt.Errorf("determine mapping %s: next is not determination %s's review: it must name the same mapping, job and pair, "+
			"with the determination's outcome, ID and time; nothing was recorded", d.MappingID, d.ID)
	}
	return nil
}

// checkStale refuses, for MarkStale, any determination but re-projection's
// valid ReviewStale one.
func checkStale(d Determination) error {
	if d.Outcome != ReviewStale || d.DecidedBy != DecidedByReproject {
		return fmt.Errorf("mark mapping %s stale: determination %s is a %q determination by %q; MarkStale records only re-projection's %q determination by %q. "+
			"Record a reviewer's determination with Determine; nothing was recorded",
			d.MappingID, d.ID, d.Outcome, d.DecidedBy, ReviewStale, DecidedByReproject)
	}
	return d.Validate()
}

// reviewChanged is the refusal of a write whose expected determination no
// longer holds; current is the review's determination ID, "" for none.
func reviewChanged(mappingID, current string) error {
	now := "it has no determination"
	if current != "" {
		now = "its determination is now " + current
	}
	return fmt.Errorf("mapping %s changed since its review was read (%s); nothing was recorded. "+
		"Reload the mapping and try again: %w", mappingID, now, ErrReviewChanged)
}

// unprojected is MarkReprojected's refusal of a review whose graph changes
// are outstanding.
func unprojected(mappingID string) error {
	return fmt.Errorf("mapping %s has a determination not yet applied to the graph, so it was not re-projected and nothing was recorded. "+
		"Record any determination (which applies outstanding ones first), then re-project: %w", mappingID, ErrUnprojected)
}

// mappingNotFound is the refusal of feedback on a pair that is not one of
// the job's mappings.
func mappingNotFound(mappingID, jobID string) error {
	return fmt.Errorf("mapping %s is not a mapping of job %s; list them with 'crosscodex results export %s'; nothing was recorded: %w",
		mappingID, jobID, jobID, ErrMappingNotFound)
}
