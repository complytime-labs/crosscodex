package agedriver

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

// CreateRequiresEdge creates a REQUIRES edge from the tenant's requires_consensus data.
// The method transforms RequiresEdge consensus metadata into a graph edge with
// full provenance (models, confidence, vote counts). This is the pipeline entry point
// for materializing consensus results into the graph.
// The edge ID is reqEdge.EdgeID(), so re-materializing the same job's result returns ErrEdgeExists.
func (c *ageClient) CreateRequiresEdge(ctx context.Context, tenant string, reqEdge graphdb.RequiresEdge) (err error) {
	if err := graphdb.CheckRequiresEdge(reqEdge); err != nil {
		return fmt.Errorf("create requires edge: %w", err)
	}
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.CreateRequiresEdge", tenant)
	defer span.End()
	defer func() { c.record(ctx, "create_requires_edge", start, err) }()
	span.SetAttributes(
		attribute.String("source.id", reqEdge.SourceID),
		attribute.String("target.id", reqEdge.TargetID),
	)

	tx, err := c.beginTx(ctx, tenant)
	if err != nil {
		failSpan(span, err)
		return err
	}
	defer func() { _ = tx.Rollback() }()

	gn := graphName(tenant)
	if err := checkEdgeWrite(ctx, tx, tenant, gn, reqEdge.SourceID, reqEdge.TargetID, reqEdge.EdgeID()); err != nil {
		markOutcome(span, err)
		return fmt.Errorf("create requires edge: %w", err)
	}

	// Build properties map for REQUIRES edge. Source/target node IDs are
	// structural topology (the MATCH clause endpoints), not data properties.
	// Tenant ID is the graph partition key (graph name), not an edge property.
	pairs := []propertyPair{
		{"id", fmt.Sprintf("'%s'", escapeCypher(reqEdge.EdgeID()))},
		{"valid_from", fmt.Sprintf("'%s'", escapeCypher(graphdb.FormatTime(reqEdge.AnalyzedAt)))},
		{"confidence", fmt.Sprintf("%g", reqEdge.Confidence)},
		{"unanimous", fmt.Sprintf("%v", reqEdge.Unanimous)},
		{"valid_votes", fmt.Sprintf("%d", reqEdge.ValidVotes)},
		{"total_votes", fmt.Sprintf("%d", reqEdge.TotalVotes)},
		{"vote_weight", fmt.Sprintf("%g", reqEdge.VoteWeight)},
	}
	if len(reqEdge.Models) > 0 {
		pairs = append(pairs, propertyPair{"models", cypherStringList(reqEdge.Models)})
	}
	pairs = append(pairs, propertyPair{"samples_per_model", fmt.Sprintf("%d", reqEdge.SamplesPerModel)})
	if reqEdge.PromptVersion != "" {
		pairs = append(pairs, propertyPair{"prompt_version", fmt.Sprintf("'%s'", escapeCypher(reqEdge.PromptVersion))})
	}
	pairs = append(pairs,
		propertyPair{"analyzed_at", fmt.Sprintf("'%s'", escapeCypher(graphdb.FormatTime(reqEdge.AnalyzedAt)))},
		propertyPair{"job_id", fmt.Sprintf("'%s'", escapeCypher(reqEdge.JobID))},
	)

	cypher := fmt.Sprintf(
		"MATCH (s {id: '%s'}), (t {id: '%s'}) CREATE (s)-[e:REQUIRES %s]->(t)",
		escapeCypher(reqEdge.SourceID),
		escapeCypher(reqEdge.TargetID),
		joinPropertyPairs(pairs),
	)
	query := cypherSQL(gn, cypher, "v agtype")

	if _, err := tx.ExecContext(ctx, query); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("create requires edge: %w", classifyErr(ctx, tenant, err))
	}
	return c.commit(span, tx, "create requires edge")
}
