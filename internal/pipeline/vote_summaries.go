package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// VoteSummaryPair is one row to insert into vote_summaries: a
// (source, target) pair the requires and/or relationship analyzer produced a
// result for, with an initial viability of 0 pending synthesis's update.
type VoteSummaryPair struct {
	SourceID   string
	TargetID   string
	Consensus  string
	Confidence float64
}

// buildVoteSummaryPairs merges the persisted requires and relationship
// results for a job into one row per distinct (source, target) pair —
// vote_summaries' primary key is (job_id, source_id, target_id), so a pair
// present in both analyzers' output must collapse to a single row.
// Relationship's RelationshipType wins as the Consensus label when both are
// present (it is the more specific classification); Confidence likewise
// prefers relationship's when both are present.
func buildVoteSummaryPairs(outputs map[string]*analyzer.Output) ([]VoteSummaryPair, error) {
	byPair := make(map[[2]string]VoteSummaryPair)
	var order [][2]string

	if out, ok := outputs["requires"]; ok && len(out.ResultData) > 0 {
		var reqResults []results.RequiresResult
		if err := json.Unmarshal(out.ResultData, &reqResults); err != nil {
			return nil, fmt.Errorf("buildVoteSummaryPairs: unmarshaling requires results: %w", err)
		}
		for _, r := range reqResults {
			key := [2]string{r.SourceID, r.TargetID}
			if _, exists := byPair[key]; !exists {
				order = append(order, key)
			}
			byPair[key] = VoteSummaryPair{SourceID: r.SourceID, TargetID: r.TargetID, Consensus: "requires", Confidence: r.Confidence}
		}
	}

	if out, ok := outputs["relationship"]; ok && len(out.ResultData) > 0 {
		var relResults []results.SemanticMatchResult
		if err := json.Unmarshal(out.ResultData, &relResults); err != nil {
			return nil, fmt.Errorf("buildVoteSummaryPairs: unmarshaling relationship results: %w", err)
		}
		for _, r := range relResults {
			key := [2]string{r.SourceID, r.TargetID}
			if _, exists := byPair[key]; !exists {
				order = append(order, key)
			}
			byPair[key] = VoteSummaryPair{SourceID: r.SourceID, TargetID: r.TargetID, Consensus: r.RelationshipType, Confidence: r.Confidence}
		}
	}

	pairs := make([]VoteSummaryPair, len(order))
	for i, key := range order {
		pairs[i] = byPair[key]
	}
	return pairs, nil
}

// writeVoteSummaries upserts vote_summaries rows with viability=0, ignoring
// conflicts (a resumed job that already wrote these rows in a prior process
// invocation must not error or attempt to mutate a row protected by the
// immutability trigger).
func writeVoteSummaries(ctx context.Context, conn db.TenantConnection, tenantID, jobID string, pairs []VoteSummaryPair) error {
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return fmt.Errorf("writeVoteSummaries: %w", err)
	}
	if len(pairs) == 0 {
		return nil
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("writeVoteSummaries: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := tx.Exec(ctx, "SELECT set_config('app.current_tenant', $1, true)", tenantID); err != nil {
		return fmt.Errorf("writeVoteSummaries: setting tenant: %w", err)
	}

	query := `
		INSERT INTO vote_summaries (job_id, source_id, target_id, consensus, confidence, viability, tenant_id)
		VALUES ($1, $2, $3, $4, $5, 0, $6)
		ON CONFLICT (job_id, source_id, target_id) DO NOTHING
	`
	for _, p := range pairs {
		if err := tx.Exec(ctx, query, jobID, p.SourceID, p.TargetID, p.Consensus, p.Confidence, tenantID); err != nil {
			return fmt.Errorf("writeVoteSummaries: inserting %s--%s: %w", p.SourceID, p.TargetID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("writeVoteSummaries: commit failed: %w", err)
	}
	return nil
}
