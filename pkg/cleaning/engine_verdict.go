package cleaning

import (
	"context"
	"fmt"
)

// VerdictEdges returns the live edge IDs of each verdict in keys, sorted, from
// both stages, non-viable and NO_RELATIONSHIP edges included, exactly as a
// cleaning pass reads them. A key without a live edge maps to nil. This is how
// a mapping reaches its graph state (the mapping identity rule): resolve the
// mapping to its verdict, then read the verdict's live edges; never follow
// edge IDs.
//
// Every key's job must be a completed job of the tenant, else the error wraps
// ErrVerdictJobNotFound and the graph is not read. Each distinct catalog pair
// the keys' jobs mapped is read once.
func (e *Engine) VerdictEdges(ctx context.Context, tenantID string, keys []VerdictKey) (_ map[VerdictKey][]string, err error) {
	ctx, finish, err := e.begin(ctx, "VerdictEdges", "verdict_edges", tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { finish(err) }()

	jobs, err := e.store.CompletedJobs(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("read verdict edges: load completed jobs: %w", err)
	}
	completed := make(map[string]Job, len(jobs))
	for _, j := range jobs {
		completed[j.ID] = j
	}
	named := map[string]Job{}
	var order []Job
	out := make(map[VerdictKey][]string, len(keys))
	for _, k := range keys {
		j, ok := completed[k.JobID]
		if !ok {
			return nil, fmt.Errorf("read verdict %s: %w: job %s is unknown or has not completed, so it has no verdicts to read. "+
				"Name a verdict of one of the tenant's completed jobs", k, ErrVerdictJobNotFound, k.JobID)
		}
		if _, seen := named[j.ID]; !seen {
			named[j.ID] = j
			order = append(order, j)
		}
		out[k] = nil
	}

	for _, cp := range catalogPairs(Scope{}, order) {
		pairs, _, err := e.loadPairs(ctx, tenantID, named, cp[0], cp[1])
		if err != nil {
			return nil, fmt.Errorf("read verdict edges: %w", err)
		}
		for _, p := range pairs {
			for _, v := range p.Verdicts {
				if _, wanted := out[v.Key]; wanted {
					out[v.Key] = v.EdgeIDs
				}
			}
		}
	}
	return out, nil
}
