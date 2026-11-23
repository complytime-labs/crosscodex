package cleaning

import (
	"context"
	"fmt"
)

// project applies every unprojected finding's graph change and marks it
// projected, returning how many it applied. A retired finding's edges are
// superseded at its latest retire decision's time, and a restored finding's
// edges are recreated under its latest restore decision, so a retry repeats
// the same change: retire skips superseded edges and restore finds the edges
// it already made. Reported, pending, dismissed, cleared and stale findings
// change no edge; an unknown status is refused. A failure part-way is a
// *ProjectionError saying how far it got and to resume.
func (e *Engine) project(ctx context.Context, tenantID string) (int, error) {
	uns, err := e.store.Unprojected(ctx, tenantID)
	if err != nil {
		return 0, fmt.Errorf("list unprojected findings: %w", err)
	}
	for i, s := range uns {
		if err := e.projectOne(ctx, tenantID, s); err != nil {
			return i, &ProjectionError{Applied: i, Total: len(uns), Err: err}
		}
	}
	return len(uns), nil
}

// ProjectionError reports a graph update that stopped part-way. It matches
// ErrProjectionIncomplete and wraps the failure. Public is its text without
// the failure, which may be a driver error, so it can be shown to an API
// caller; Error adds the failure for logs.
type ProjectionError struct {
	// Applied and Total count the findings applied to the graph and the
	// findings there were to apply.
	Applied, Total int
	// DecisionID is set when the request recorded a reviewer decision before
	// the update stopped: that decision stands.
	DecisionID string
	// Err is the failure that stopped the update.
	Err error
}

// Public states what was recorded, how far the graph update got and how to
// finish it.
func (e *ProjectionError) Public() string {
	msg := fmt.Sprintf("%v: %d of %d findings applied to the graph; %s to finish", ErrProjectionIncomplete, e.Applied, e.Total, resumeHint)
	if e.DecisionID != "" {
		msg = fmt.Sprintf("decision %s was recorded and stands, but %s", e.DecisionID, msg)
	}
	return msg
}

func (e *ProjectionError) Error() string { return e.Public() + ": " + e.Err.Error() }

// Unwrap returns ErrProjectionIncomplete and the failure.
func (e *ProjectionError) Unwrap() []error { return []error{ErrProjectionIncomplete, e.Err} }

func (e *Engine) projectOne(ctx context.Context, tenantID string, s StoredFinding) error {
	edges := s.EdgeIDs
	switch s.Status {
	case StatusRetired:
		d, err := e.latestDecision(ctx, tenantID, s, VerdictRetire)
		if err != nil {
			return err
		}
		if _, err := RetireEdges(ctx, e.g, tenantID, s.EdgeIDs, d.DecidedAt); err != nil {
			return fmt.Errorf("finding %s: %w", s.ID, err)
		}
	case StatusRestored:
		d, err := e.latestDecision(ctx, tenantID, s, VerdictRestore)
		if err != nil {
			return err
		}
		if edges, err = RestoreEdges(ctx, e.g, tenantID, s.EdgeIDs, d.ID, d.DecidedAt); err != nil {
			return fmt.Errorf("finding %s: %w", s.ID, err)
		}
	case StatusReported, StatusPending, StatusDismissed, StatusCleared, StatusStale:
		// No graph change: only the row is marked projected.
	default:
		return fmt.Errorf("finding %s has unknown status %q, so its graph change is unknown and nothing was applied for it. "+
			"Its edge_findings row was changed outside crosscodex; set its status back to one of %s from its decision history",
			s.ID, s.Status, joinOr(statuses))
	}
	if err := e.store.MarkProjected(ctx, tenantID, s.ID, edges); err != nil {
		return fmt.Errorf("finding %s: %w", s.ID, err)
	}
	return nil
}

// latestDecision returns the newest decision with verdict on finding s. A
// retired or restored finding always has one, because only a decision moves
// a finding there.
func (e *Engine) latestDecision(ctx context.Context, tenantID string, s StoredFinding, verdict string) (Decision, error) {
	_, ds, err := e.store.GetFinding(ctx, tenantID, s.ID)
	if err != nil {
		return Decision{}, fmt.Errorf("finding %s: load its decisions: %w", s.ID, err)
	}
	for i := len(ds) - 1; i >= 0; i-- {
		if ds[i].Verdict == verdict {
			return ds[i], nil
		}
	}
	return Decision{}, fmt.Errorf("finding %s is %q but has no %s decision, so its graph change has no recorded time or author. "+
		"Its edge_findings row was changed outside crosscodex; restore its status from its decision history", s.ID, s.Status, verdict)
}
