package cleaning

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

// ErrEdgeNotLive marks an edge that is missing from the graph or already
// superseded, where a live edge was required.
var ErrEdgeNotLive = errors.New("edge is not live")

// ErrEdgeLive marks a live edge where a retired one was required.
var ErrEdgeLive = errors.New("edge is live")

// restoredEdgeKind tags the IDs of restored edges so they cannot collide with
// IDs derived for other purposes.
const restoredEdgeKind = "semantic-match-restored"

// RetireEdges supersedes each live edge at at and returns the IDs it
// superseded, in input order. An edge that is already superseded is skipped,
// because a second SupersedeFact would overwrite its valid_to. A missing edge
// is an error wrapping graphdb.ErrEdgeNotFound. On error the IDs superseded so
// far are returned with it; calling again with the same IDs finishes the job.
//
// These functions are the graph mutation of edge cleaning. They take no
// finding type so that FeedbackService (#230) can retire a mapping's verdicts
// through them.
func RetireEdges(ctx context.Context, g graphdb.GraphDB, tenantID string, edgeIDs []string, at time.Time) ([]string, error) {
	var retired []string
	for _, id := range edgeIDs {
		e, err := g.GetEdge(ctx, tenantID, id)
		if err != nil {
			return retired, fmt.Errorf("retire edge %q: %w", id, err)
		}
		if e.ValidTo != nil {
			continue
		}
		if at.Before(e.ValidFrom) {
			return retired, fmt.Errorf("retire edge %q: retirement time %s is before the edge became valid at %s, "+
				"which would invert its validity interval. Retire at or after valid_from",
				id, at.UTC().Format(time.RFC3339), e.ValidFrom.UTC().Format(time.RFC3339))
		}
		// Check-then-act is not serialized: the engine's unprojected-findings
		// guard is itself check-then-act, so a Decide and an Apply can project
		// at once. For one finding that is harmless, because the time comes
		// from its decision. When two findings share this edge and project
		// concurrently, the later supersede may overwrite valid_to with the
		// other decision's time.
		found, err := g.SupersedeFact(ctx, tenantID, graphdb.SupersedeRequest{EdgeID: id, SupersededAt: at})
		if err != nil {
			return retired, fmt.Errorf("retire edge %q: %w", id, err)
		}
		if !found {
			return retired, fmt.Errorf("retire edge %q: %w", id, graphdb.ErrEdgeNotFound)
		}
		retired = append(retired, id)
	}
	return retired, nil
}

// RestoreEdges recreates each retired edge as a new live edge starting at at
// and returns the IDs of the live successors in input order. The new edge has
// the retired edge's endpoints, label, properties, determination fields and
// confidence, with Supersedes set to the retired ID, valid_from set to at (so
// an as-of query inside the retired gap does not show the mapping as live) and
// the ID graphdb.DerivedID("semantic-match-restored", retiredID, decisionID).
// Pass the restore decision's decided-at time so that retries are
// deterministic.
//
// Restoring is idempotent per retired edge. A live edge whose Supersedes is the
// retired ID, from this or any other decision, counts as done and its ID is
// returned instead of creating a second successor; a repeat keeps the first
// restore's valid_from whatever at it passes. An existing edge with the derived
// ID also counts as done (graphdb.ErrEdgeExists).
//
// Every input is checked before anything is written. A missing edge wraps
// graphdb.ErrEdgeNotFound; a live edge wraps ErrEdgeLive; a zero at, or an at
// before a retired edge's valid_to, is an error.
func RestoreEdges(ctx context.Context, g graphdb.GraphDB, tenantID string, retiredEdgeIDs []string, decisionID string, at time.Time) ([]string, error) {
	if decisionID == "" {
		return nil, errors.New("restore edges: decision ID is required, because it makes the restored edge IDs repeatable")
	}
	if at.IsZero() {
		return nil, errors.New("restore edges: the restore time is required, because it becomes the restored edges' valid_from")
	}
	retired := make([]*graphdb.EdgeWithEndpoints, len(retiredEdgeIDs))
	successors := make([]string, len(retiredEdgeIDs))
	for i, id := range retiredEdgeIDs {
		e, err := g.GetEdge(ctx, tenantID, id)
		if err != nil {
			return nil, fmt.Errorf("restore edge %q: %w", id, err)
		}
		if e.ValidTo == nil {
			return nil, fmt.Errorf("restore edge %q: %w. Only a retired edge can be restored; nothing was written", id, ErrEdgeLive)
		}
		if successors[i], err = liveSuccessor(ctx, g, tenantID, e); err != nil {
			return nil, err
		}
		if successors[i] == "" && at.Before(*e.ValidTo) {
			return nil, fmt.Errorf("restore edge %q: restore time %s is before the edge was retired at %s, "+
				"which would make the mapping live twice. Restore at or after the retirement; nothing was written",
				id, at.UTC().Format(time.RFC3339), e.ValidTo.UTC().Format(time.RFC3339))
		}
		retired[i] = e
	}

	reserved := graphdb.ReservedEdgeKeys()
	for i, old := range retired {
		if successors[i] != "" {
			continue
		}
		props := maps.Clone(old.Properties)
		for k := range reserved {
			delete(props, k)
		}
		successors[i] = graphdb.DerivedID(restoredEdgeKind, old.ID, decisionID)
		err := g.CreateEdge(ctx, tenantID, old.SourceID, old.TargetID, graphdb.Edge{
			ID:                successors[i],
			Label:             old.Label,
			Properties:        props,
			ValidFrom:         at,
			DeterminedBy:      old.DeterminedBy,
			DeterminationType: old.DeterminationType,
			Confidence:        old.Confidence,
			Supersedes:        old.ID,
		})
		if err != nil && !errors.Is(err, graphdb.ErrEdgeExists) {
			return nil, fmt.Errorf("restore edge %q: %w", old.ID, err)
		}
	}
	return successors, nil
}

// liveSuccessor returns the ID of a live edge from old's source that
// supersedes old, or "" when there is none. The query filters on the stored
// supersedes property and the source node's id, so it reads only the edges of
// one node's restore history; it is not a traversal and has no depth to bound.
func liveSuccessor(ctx context.Context, g graphdb.GraphDB, tenantID string, old *graphdb.EdgeWithEndpoints) (string, error) {
	rels, err := g.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{
		EdgeLabel:        old.Label,
		Properties:       map[string]any{"supersedes": old.ID},
		SourceProperties: map[string]any{"id": old.SourceID},
	})
	if err != nil {
		return "", fmt.Errorf("restore edge %q: look for an existing restore: %w", old.ID, err)
	}
	var found []string
	for _, r := range rels {
		if r.Edge.ValidTo == nil && r.Target.ID == old.TargetID {
			found = append(found, r.Edge.ID)
		}
	}
	if len(found) == 0 {
		return "", nil
	}
	slices.Sort(found)
	return found[0], nil
}

// CheckLive returns an error wrapping ErrEdgeNotLive that names the first edge
// that is missing or superseded. Any other graph failure is returned as is, so
// an outage is not reported as a missing edge.
func CheckLive(ctx context.Context, g graphdb.GraphDB, tenantID string, edgeIDs []string) error {
	for _, id := range edgeIDs {
		e, err := g.GetEdge(ctx, tenantID, id)
		if errors.Is(err, graphdb.ErrEdgeNotFound) {
			return fmt.Errorf("edge %q: %w: it is missing from the graph", id, ErrEdgeNotLive)
		}
		if err != nil {
			return fmt.Errorf("check edge %q: %w", id, err)
		}
		if e.ValidTo != nil {
			return fmt.Errorf("edge %q: %w: it was superseded at %s", id, ErrEdgeNotLive, e.ValidTo.UTC().Format(time.RFC3339))
		}
	}
	return nil
}
