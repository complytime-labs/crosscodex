package agedriver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

// CreateRequiresEdge creates a REQUIRES edge from the tenant's requires_consensus data.
// The method transforms RequiresEdge consensus metadata into a graph edge with
// full provenance (models, confidence, vote counts). This is the pipeline entry point
// for materializing consensus results into the graph.
func (c *ageClient) CreateRequiresEdge(ctx context.Context, tenant string, reqEdge graphdb.RequiresEdge) error {
	if reqEdge.SourceID == "" {
		return fmt.Errorf("create requires edge: source_id is required")
	}
	if reqEdge.TargetID == "" {
		return fmt.Errorf("create requires edge: target_id is required")
	}
	if reqEdge.AnalyzedAt.IsZero() {
		return fmt.Errorf("create requires edge: analyzed_at is required")
	}
	start := time.Now()
	ctx, span := c.startSpan(ctx, "graphdb.CreateRequiresEdge")
	defer span.End()
	span.SetAttributes(
		attribute.String("tenant.id", tenant),
		attribute.String("source.id", reqEdge.SourceID),
		attribute.String("target.id", reqEdge.TargetID),
	)

	tx, err := c.beginTx(ctx, tenant)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	defer func() { _ = tx.Rollback() }()

	gn := graphName(tenant)

	// Build properties map for REQUIRES edge. Source/target node IDs are
	// structural topology (the MATCH clause endpoints), not data properties.
	// Tenant ID is the graph partition key (graph name), not an edge property.
	var pairs []string
	pairs = append(pairs, fmt.Sprintf("confidence: %g", reqEdge.Confidence))
	pairs = append(pairs, fmt.Sprintf("unanimous: %v", reqEdge.Unanimous))
	pairs = append(pairs, fmt.Sprintf("valid_votes: %d", reqEdge.ValidVotes))
	pairs = append(pairs, fmt.Sprintf("total_votes: %d", reqEdge.TotalVotes))
	pairs = append(pairs, fmt.Sprintf("vote_weight: %g", reqEdge.VoteWeight))

	if len(reqEdge.Models) > 0 {
		models := ""
		for i, m := range reqEdge.Models {
			if i > 0 {
				models += ", "
			}
			models += "'" + escapeCypher(m) + "'"
		}
		pairs = append(pairs, fmt.Sprintf("models: [%s]", models))
	}

	pairs = append(pairs, fmt.Sprintf("samples_per_model: %d", reqEdge.SamplesPerModel))
	if reqEdge.PromptVersion != "" {
		pairs = append(pairs, fmt.Sprintf("prompt_version: '%s'", escapeCypher(reqEdge.PromptVersion)))
	}
	pairs = append(pairs, fmt.Sprintf("analyzed_at: '%s'", escapeCypher(reqEdge.AnalyzedAt.Format(time.RFC3339Nano))))
	pairs = append(pairs, fmt.Sprintf("job_id: '%s'", escapeCypher(reqEdge.JobID)))

	props := "{" + strings.Join(pairs, ", ") + "}"

	cypher := fmt.Sprintf(
		"MATCH (s {id: '%s'}), (t {id: '%s'}) CREATE (s)-[e:REQUIRES %s]->(t)",
		escapeCypher(reqEdge.SourceID),
		escapeCypher(reqEdge.TargetID),
		props,
	)
	query := fmt.Sprintf(
		"SELECT * FROM ag_catalog.cypher('%s', "+cypherDollarTag+" %s "+cypherDollarTag+") AS (v agtype)",
		escapeCypher(gn), cypher,
	)

	if _, err := tx.ExecContext(ctx, query); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("create requires edge: %w", err)
	}
	if c.queryCounter != nil {
		c.queryCounter.Add(ctx, 1)
	}
	if c.queryLatency != nil {
		c.queryLatency.Record(ctx, time.Since(start).Milliseconds())
	}
	span.SetStatus(codes.Ok, "")
	return tx.Commit()
}
