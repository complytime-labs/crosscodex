package feedback

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// ReprojectItem is one review re-projection judged.
type ReprojectItem struct {
	MappingID string
	// Status is the review's status as read: ReviewInvalid, or ReviewValid
	// after a restore.
	Status ReviewStatus
	Action cleaning.ReprojectAction
	// EdgeIDs are the edge IDs the review stores after the action, sorted.
	EdgeIDs []string
}

// ReprojectReport is what a re-projection did, or with dryRun would do.
type ReprojectReport struct {
	// Items holds one entry per invalid review and valid review that
	// restored edges, in mapping ID order.
	Items []ReprojectItem
	// Counts of Items by action.
	Retired, Relinked, Staled, InSync int
}

// staleRequest is the canonical request of re-projection's stale
// determination. Its SHA-256 is the determination ID, and the signed
// feedback.reproject link names it.
type staleRequest struct {
	Tenant                string   `json:"tenant"`
	JobID                 string   `json:"job_id"`
	MappingID             string   `json:"mapping_id"`
	Verdict               string   `json:"verdict"`
	Outcome               string   `json:"outcome"`
	Reason                string   `json:"reason"`
	PreviousDetermination string   `json:"previous_determination"`
	EdgeIDs               []string `json:"edge_ids"`
	DecidedAt             string   `json:"decided_at"`
}

// Reproject replays the tenant's reviews into the graph, for use after the
// graph was rebuilt from the relational record, as cleaning's Reproject does
// for findings. It calls no LLM. For each invalid review, and each valid one
// whose determination restored edges, in mapping ID order, it reads the
// mapping's verdict's live edges (VerdictEdges; none when the job is no
// longer completed), which of the review's stored edge IDs still exist, and
// whether the mapping's original edge (ID mappingID) exists:
//
//   - invalid with a live edge (rebuilt, or live again): the edges join its
//     edge IDs and are superseded at the determination's time, or at an
//     edge's valid_from when that is later (cleaning.ReprojectRetire);
//   - valid with live edges that are not its edge IDs, or either with stored
//     edge IDs gone while others remain: its edge IDs become the live edges,
//     or the remaining ones (cleaning.ReprojectRelink);
//   - no live edge, no stored edge ID, no original edge, and no edge of the
//     verdict at all in the graph, live or superseded, under any edge ID
//     (cleaning.PresentVerdicts): the graph was rebuilt from new jobs, so the
//     review is marked stale with a signed determination naming the missing
//     verdict (cleaning.ReprojectStale);
//   - anything else is consistent and changes nothing (cleaning.ReprojectInSync).
//     This includes a verdict whose only edges cleaning re-projection retired,
//     governed there by a later finding decision.
//
// A second run therefore reports every remaining review in sync. With dryRun
// it computes the same report and writes nothing. It is refused without
// WithDeterminations, while any review is unprojected, and without a signer
// unless dryRun, before anything is written. Every write is checked against
// the determination read, so a determination recorded meanwhile stops the
// run with ErrReviewChanged naming the mapping. A failure part-way leaves the
// reviews before it re-projected; running it again finishes the rest.
func (s *Service) Reproject(ctx context.Context, tenantID string, dryRun bool) (_ ReprojectReport, err error) {
	ctx, finish := s.begin(ctx, "Reproject", "reproject")
	defer func() { finish(err) }()
	setTenant(ctx, tenantID)

	ctxTenant, err := tenant.FromContext(ctx)
	switch {
	case err != nil:
		return ReprojectReport{}, fmt.Errorf("feedback reproject: the request carries no tenant, so it was refused and nothing was read or written: %w", err)
	case ctxTenant != tenantID:
		// Neither tenant is named: the message must not reveal another tenant.
		return ReprojectReport{}, fmt.Errorf("feedback reproject: %w", cleaning.ErrTenantMismatch)
	}
	if s.g == nil || s.verdicts == nil {
		return ReprojectReport{}, fmt.Errorf("reproject mapping determinations: %w", ErrDeterminationsUnavailable)
	}
	slog.InfoContext(ctx, "feedback reproject started", "tenant.id", tenantID, "dry_run", dryRun)

	uns, err := s.store.Unprojected(ctx, tenantID)
	if err != nil {
		return ReprojectReport{}, fmt.Errorf("reproject mapping determinations: list unprojected ones: %w", err)
	}
	if len(uns) > 0 {
		return ReprojectReport{}, fmt.Errorf("%d mapping determinations are recorded but not yet applied to the graph, so re-projection was refused "+
			"and nothing was changed. They are applied before the next determination is recorded; check the daemon log for the graph error "+
			"that stopped them, fix it, record a determination or retry, then rerun --reproject: %w", len(uns), ErrUnprojected)
	}
	if !dryRun && s.signer == nil {
		return ReprojectReport{}, fmt.Errorf("reproject mapping determinations: %w", ErrSigningDisabled)
	}
	reviews, err := s.store.Reviews(ctx, tenantID)
	if err != nil {
		return ReprojectReport{}, fmt.Errorf("reproject mapping determinations: %w", err)
	}
	var todo []Review
	for _, r := range reviews {
		if r.Status == ReviewInvalid || (r.Status == ReviewValid && len(r.EdgeIDs) > 0) {
			todo = append(todo, r)
		}
	}
	liveOf, err := s.liveEdges(ctx, tenantID, todo)
	if err != nil {
		return ReprojectReport{}, fmt.Errorf("reproject mapping determinations: %w", err)
	}

	var rep ReprojectReport
	for i, r := range todo {
		item, err := s.reprojectOne(ctx, tenantID, r, liveOf[verdictKey(r)], dryRun)
		if errors.Is(err, ErrReviewChanged) {
			err = fmt.Errorf("mapping %s changed during re-projection, so nothing was recorded for it; rerun --reproject to judge its current state: %w",
				r.MappingID, err)
		}
		if err != nil {
			return ReprojectReport{}, fmt.Errorf("reproject mapping %s: %d of %d reviews were re-projected before it and stand; "+
				"run the re-projection again to finish, it repeats nothing already done: %w", r.MappingID, i, len(todo), err)
		}
		rep.Items = append(rep.Items, item)
		switch item.Action {
		case cleaning.ReprojectRetire:
			rep.Retired++
		case cleaning.ReprojectRelink:
			rep.Relinked++
		case cleaning.ReprojectStale:
			rep.Staled++
		case cleaning.ReprojectInSync:
			rep.InSync++
		}
	}
	slog.InfoContext(ctx, "feedback reproject finished", "tenant.id", tenantID, "dry_run", dryRun, "reviews", len(rep.Items),
		"retired", rep.Retired, "relinked", rep.Relinked, "staled", rep.Staled, "in_sync", rep.InSync)
	return rep, nil
}

func verdictKey(r Review) cleaning.VerdictKey {
	return cleaning.VerdictKey{JobID: r.JobID, SourceID: r.SourceID, TargetID: r.TargetID}
}

// liveEdges returns the live edges of every review's verdict, read one job
// at a time. A job VerdictEdges reports as not completed (retention may have
// purged it) has no verdicts left to read: its reviews are left out of the
// map, so they have no live edge and are judged by their stored edges alone.
func (s *Service) liveEdges(ctx context.Context, tenantID string, reviews []Review) (map[cleaning.VerdictKey][]string, error) {
	byJob := map[string][]cleaning.VerdictKey{}
	var jobs []string
	for _, r := range reviews {
		if _, seen := byJob[r.JobID]; !seen {
			jobs = append(jobs, r.JobID)
		}
		byJob[r.JobID] = append(byJob[r.JobID], verdictKey(r))
	}
	out := map[cleaning.VerdictKey][]string{}
	for _, job := range jobs {
		live, err := s.verdicts.VerdictEdges(ctx, tenantID, byJob[job])
		if errors.Is(err, cleaning.ErrVerdictJobNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read job %s's live edges: %w", job, err)
		}
		for k, ids := range live {
			out[k] = ids
		}
	}
	return out, nil
}

// reprojectOne judges one review against its verdict's live edges and,
// unless dryRun, applies the change. See Reproject.
func (s *Service) reprojectOne(ctx context.Context, tenantID string, r Review, live []string, dryRun bool) (ReprojectItem, error) {
	stored := sortedUnique(r.EdgeIDs)
	existing, err := cleaning.ExistingEdges(ctx, s.g, tenantID, stored)
	if err != nil {
		return ReprojectItem{}, err
	}
	original := slices.Contains(existing, r.MappingID)
	if !original {
		found, err := cleaning.ExistingEdges(ctx, s.g, tenantID, []string{r.MappingID})
		if err != nil {
			return ReprojectItem{}, err
		}
		original = len(found) > 0
	}
	live = sortedUnique(live)
	gone := len(live) == 0 && len(existing) == 0 && !original
	if gone {
		// The verdict may still be in the graph under edge IDs the review
		// never stored, none of them live: cleaning re-projection retired
		// them, or retention purged the job. Then it is not gone.
		present, err := cleaning.PresentVerdicts(ctx, s.g, tenantID, []cleaning.VerdictKey{verdictKey(r)})
		if err != nil {
			return ReprojectItem{}, err
		}
		gone = !present[verdictKey(r)]
	}

	item := ReprojectItem{MappingID: r.MappingID, Status: r.Status, Action: cleaning.ReprojectInSync, EdgeIDs: stored}
	next := stored
	switch {
	case gone:
		item.Action = cleaning.ReprojectStale
		if !dryRun {
			if err := s.markStale(ctx, tenantID, r, stored); err != nil {
				return ReprojectItem{}, err
			}
		}
		return item, nil
	case r.Status == ReviewInvalid && len(live) > 0:
		next = sortedUnique(append(slices.Clone(existing), live...))
		item.Action, item.EdgeIDs = cleaning.ReprojectRetire, next
		if !dryRun {
			// Recorded before the retire: should the retire fail, the stored
			// edge IDs name the still-live edges, so a rerun finds and retires
			// them. The other order would leave edge IDs the graph does not
			// hold, and a rerun could find nothing and mark the review stale.
			if err := s.store.MarkReprojected(ctx, tenantID, r.MappingID, r.DeterminationID, next); err != nil {
				return ReprojectItem{}, err
			}
			if _, _, err := cleaning.RetireRebuiltEdges(ctx, s.g, tenantID, live, r.DecidedAt); err != nil {
				return ReprojectItem{}, err
			}
		}
		return item, nil
	case r.Status == ReviewValid && len(live) > 0:
		next = live
	case len(existing) > 0:
		next = existing
	}
	if !slices.Equal(next, stored) {
		item.Action, item.EdgeIDs = cleaning.ReprojectRelink, next
		if !dryRun {
			if err := s.store.MarkReprojected(ctx, tenantID, r.MappingID, r.DeterminationID, next); err != nil {
				return ReprojectItem{}, err
			}
		}
	}
	return item, nil
}

// markStale signs and records re-projection's stale determination on r,
// whose verdict and edges the graph no longer holds.
func (s *Service) markStale(ctx context.Context, tenantID string, r Review, stored []string) error {
	key := verdictKey(r)
	reason := fmt.Sprintf("the graph no longer holds verdict %s or any edge this review stored: it was rebuilt from new jobs. "+
		"Review the new jobs' mappings instead", key)
	decidedAt := s.stamp()
	id, raw, link, err := s.sign(ctx, "feedback.reproject", r.MappingID, staleRequest{
		Tenant: tenantID, JobID: r.JobID, MappingID: r.MappingID, Verdict: key.String(), Outcome: string(ReviewStale),
		Reason: reason, PreviousDetermination: r.DeterminationID, EdgeIDs: stored, DecidedAt: decidedAt.Format(time.RFC3339Nano),
	})
	if err != nil {
		return fmt.Errorf("mark mapping %s stale: %w", r.MappingID, err)
	}
	d := Determination{
		ID: id, MappingID: r.MappingID, JobID: r.JobID, SourceID: r.SourceID, TargetID: r.TargetID, Outcome: ReviewStale,
		DecidedBy: DecidedByReproject, Rationale: reason, Request: raw, Attestation: link, DecidedAt: decidedAt,
	}
	if err := s.store.MarkStale(ctx, tenantID, d, r.DeterminationID); err != nil {
		return err
	}
	slog.InfoContext(ctx, "mapping review marked stale", "tenant.id", tenantID, "mapping", r.MappingID, "determination", id)
	return nil
}

// sortedUnique returns ids sorted without repeats, nil for none.
func sortedUnique(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	return slices.Compact(slices.Sorted(slices.Values(ids)))
}
