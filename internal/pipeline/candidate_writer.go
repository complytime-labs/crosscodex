package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/complytime-labs/crosscodex/internal/analyzer/relationship"
	"github.com/complytime-labs/crosscodex/internal/analyzer/requires"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// WriteRequiresCandidates upserts candidate prerequisite pairs for a job.
// Idempotent: re-running candidate generation for the same job replaces
// each pair's score and provenance rather than duplicating rows.
func WriteRequiresCandidates(ctx context.Context, conn db.TenantConnection, tenantID, jobID string, pairs []requires.RequiresPair) error {
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return fmt.Errorf("WriteRequiresCandidates: %w", err)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("WriteRequiresCandidates: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	query := `
		INSERT INTO requires_candidates (tenant_id, job_id, source_id, target_id, aggregate_score, provenance)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, job_id, source_id, target_id)
		DO UPDATE SET aggregate_score = EXCLUDED.aggregate_score, provenance = EXCLUDED.provenance, created_at = NOW()
	`
	for _, p := range pairs {
		provenanceJSON, err := json.Marshal(p.Provenance)
		if err != nil {
			return fmt.Errorf("WriteRequiresCandidates: marshaling provenance for %s--%s: %w", p.SourceControlID, p.TargetControlID, err)
		}
		if err := tx.Exec(ctx, query, tenantID, jobID, p.SourceControlID, p.TargetControlID, p.AggregateScore, provenanceJSON); err != nil {
			return fmt.Errorf("WriteRequiresCandidates: upserting %s--%s: %w", p.SourceControlID, p.TargetControlID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("WriteRequiresCandidates: commit failed: %w", err)
	}
	return nil
}

// WriteRelationshipCandidates upserts candidate similarity pairs for a job.
// Idempotent: re-running candidate generation for the same job replaces
// each pair's score and provenance rather than duplicating rows.
//
// provenance is keyed by "<SourceControlID>--<TargetControlID>" and holds
// pre-marshaled JSON for that pair; pairs with no matching entry are stored
// with an empty JSON array, matching the column's default. Unlike
// WriteRequiresCandidates, provenance here is supplied out-of-band rather
// than carried on relationship.CandidatePair, which has no Provenance field.
func WriteRelationshipCandidates(ctx context.Context, conn db.TenantConnection, tenantID, jobID string, pairs []relationship.CandidatePair, provenance map[string][]byte) error {
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return fmt.Errorf("WriteRelationshipCandidates: %w", err)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("WriteRelationshipCandidates: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	query := `
		INSERT INTO relationship_candidates (tenant_id, job_id, source_id, target_id, similarity_score, provenance)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, job_id, source_id, target_id)
		DO UPDATE SET similarity_score = EXCLUDED.similarity_score, provenance = EXCLUDED.provenance, created_at = NOW()
	`
	for _, p := range pairs {
		key := p.SourceControlID + "--" + p.TargetControlID
		prov := provenance[key]
		if prov == nil {
			prov = []byte(`[]`)
		}
		if err := tx.Exec(ctx, query, tenantID, jobID, p.SourceControlID, p.TargetControlID, p.SimilarityScore, prov); err != nil {
			return fmt.Errorf("WriteRelationshipCandidates: upserting %s: %w", key, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("WriteRelationshipCandidates: commit failed: %w", err)
	}
	return nil
}
