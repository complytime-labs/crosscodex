package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

// materializeSynthesis projects a job's synthesized mappings (vote_summaries,
// the authoritative copy) as SEMANTIC_MATCH edges tagged stage=synthesized.
// Edges go in chunks of s.maxBulkEdges through BulkCreateEdges, which is
// all-or-nothing. When a chunk fails because one edge already exists (a
// redelivered event) or lacks an endpoint, nothing from the chunk was
// written, so the chunk is replayed edge by edge with the same tolerance as
// the relationship materializer.
func (s *Service) materializeSynthesis(ctx context.Context, tenantID, jobID string, data []byte) error {
	var mappings []results.SynthesizedMapping
	if err := json.Unmarshal(data, &mappings); err != nil {
		return fmt.Errorf("unmarshal synthesis results: %w", err)
	}

	now := time.Now().UTC()
	edges := make([]graphdb.BulkEdge, len(mappings))
	for i, m := range mappings {
		edges[i] = graphdb.BulkEdge{
			SourceID: m.SourceID,
			TargetID: m.TargetID,
			Edge: graphdb.Edge{
				ID:        graphdb.DerivedID(results.SemanticMatchSynthesizedIDKind, jobID, m.SourceID, m.TargetID),
				Label:     results.SemanticMatchEdgeLabel,
				ValidFrom: now,
				Properties: map[string]any{
					"relationship_type":   m.RelationshipType,
					"job_id":              jobID,
					results.StageProperty: results.StageSynthesized,
					"viability":           m.Viability,
					"is_viable":           m.IsViable,
				},
				Confidence: m.Confidence,
			},
		}
		if m.Critique != nil {
			critiqueProperties(*m.Critique, edges[i].Edge.Properties)
		}
	}

	for start := 0; start < len(edges); start += s.maxBulkEdges {
		chunk := edges[start:min(start+s.maxBulkEdges, len(edges))]
		_, err := s.graph.BulkCreateEdges(ctx, tenantID, chunk)
		if err == nil {
			continue
		}
		var bulkErr *graphdb.BulkEdgeError
		if !errors.As(err, &bulkErr) || (!isNodeExists(err) && !errors.Is(err, graphdb.ErrNodeNotFound)) {
			return fmt.Errorf("bulk create synthesized SEMANTIC_MATCH edges: %w", err)
		}
		for _, e := range chunk {
			err := s.graph.CreateEdge(ctx, tenantID, e.SourceID, e.TargetID, e.Edge)
			if err != nil && !isNodeExists(err) && !s.skipMissingEndpoint(ctx, tenantID, results.SynthesisStage, results.SemanticMatchEdgeLabel, e.SourceID, e.TargetID, err) {
				return fmt.Errorf("create synthesized SEMANTIC_MATCH edge %s->%s: %w", e.SourceID, e.TargetID, err)
			}
		}
	}
	return nil
}
