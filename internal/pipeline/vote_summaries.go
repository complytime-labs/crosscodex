package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/complytime-labs/crosscodex/internal/synthesis"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// VoteSummaryPair is one row to insert into vote_summaries: one analyzer's
// contribution to a (source, target) pair, with an initial viability of 0
// pending synthesis's update. Analyzer is the vote_summaries.analyzer_name:
// the analyzer that produced the result ("requires" or "relationship"). It
// matches the analyzer of the synthesis row that buildSynthesisInputs builds
// from the same result, which is how synthesis finds the row to update.
type VoteSummaryPair struct {
	SourceID   string
	TargetID   string
	Analyzer   string
	Consensus  string
	Confidence float64
}

// buildVoteSummaryPairs builds one vote_summaries row per persisted requires
// and relationship result for a job. A pair both analyzers produced keeps
// both rows: buildSynthesisInputs emits one synthesis row per contribution
// and synthesis updates one vote_summaries row per synthesis row, so the
// rows are keyed by (source, target, analyzer). Requires rows carry
// synthesis.ConsensusRequires as Consensus; relationship rows carry their
// RelationshipType.
//
// One analyzer listing the same pair twice is corrupt persisted output: its
// rows would collide on the key and synthesis would rank the pair twice.
// That is an error, like unparseable result data, rather than a silent pick.
func buildVoteSummaryPairs(outputs map[string]*analyzer.Output) ([]VoteSummaryPair, error) {
	var pairs []VoteSummaryPair
	seen := make(map[[3]string]struct{})
	add := func(p VoteSummaryPair) error {
		key := [3]string{p.SourceID, p.TargetID, p.Analyzer}
		if _, dup := seen[key]; dup {
			return fmt.Errorf("buildVoteSummaryPairs: %s results list pair %s--%s more than once, so the persisted analysis result is corrupt. "+
				"Synthesis cannot rank a pair twice; create a new job to re-run the analysis instead of resuming this one",
				p.Analyzer, p.SourceID, p.TargetID)
		}
		seen[key] = struct{}{}
		pairs = append(pairs, p)
		return nil
	}

	if out, ok := outputs[results.RequiresAnalyzer]; ok && len(out.ResultData) > 0 {
		var reqResults []results.RequiresResult
		if err := json.Unmarshal(out.ResultData, &reqResults); err != nil {
			return nil, fmt.Errorf("buildVoteSummaryPairs: unmarshaling requires results: %w", err)
		}
		for _, r := range reqResults {
			if err := add(VoteSummaryPair{
				SourceID: r.SourceID, TargetID: r.TargetID, Analyzer: results.RequiresAnalyzer,
				Consensus: synthesis.ConsensusRequires, Confidence: r.Confidence,
			}); err != nil {
				return nil, err
			}
		}
	}

	if out, ok := outputs[results.RelationshipAnalyzer]; ok && len(out.ResultData) > 0 {
		var relResults []results.SemanticMatchResult
		if err := json.Unmarshal(out.ResultData, &relResults); err != nil {
			return nil, fmt.Errorf("buildVoteSummaryPairs: unmarshaling relationship results: %w", err)
		}
		for _, r := range relResults {
			if err := add(VoteSummaryPair{
				SourceID: r.SourceID, TargetID: r.TargetID, Analyzer: results.RelationshipAnalyzer,
				Consensus: r.RelationshipType, Confidence: r.Confidence,
			}); err != nil {
				return nil, err
			}
		}
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

	if err := tx.Exec(ctx, "SELECT public.enter_tenant($1)", tenantID); err != nil {
		return fmt.Errorf("writeVoteSummaries: setting tenant: %w", err)
	}

	query := `
		INSERT INTO vote_summaries (job_id, source_id, target_id, analyzer_name, consensus, confidence, viability, tenant_id)
		VALUES ($1, $2, $3, $4, $5, $6, 0, $7)
		ON CONFLICT (job_id, source_id, target_id, analyzer_name) DO NOTHING
	`
	for _, p := range pairs {
		if err := tx.Exec(ctx, query, jobID, p.SourceID, p.TargetID, p.Analyzer, p.Consensus, p.Confidence, tenantID); err != nil {
			return fmt.Errorf("writeVoteSummaries: inserting %s--%s (%s): %w", p.SourceID, p.TargetID, p.Analyzer, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("writeVoteSummaries: commit failed: %w", err)
	}
	return nil
}
