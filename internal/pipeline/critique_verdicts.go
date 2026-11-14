package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/complytime-labs/crosscodex/internal/analyzer/critique"
	"github.com/complytime-labs/crosscodex/internal/analyzer/relationship"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// requiresVerdict is the Verdict of a proposed requires verdict: the source
// requires the target.
const requiresVerdict = "REQUIRES"

// analysisResultReader reads a job's stored analyzer results, as
// Store.GetCompletedAnalysisResults does, for the tenant in ctx.
type analysisResultReader interface {
	GetCompletedAnalysisResults(ctx context.Context, jobID string, analyzerNames []string) (map[string]*analyzer.Output, error)
}

// CritiqueVerdictProvider implements critique.VerdictProvider from a job's
// stored relationship and requires results. Those analyzers store only the
// verdicts they accepted: related pairs, and REQUIRES pairs that cleared
// their threshold. A disabled analyzer has no stored result and contributes
// nothing.
type CritiqueVerdictProvider struct {
	results analysisResultReader
}

var _ critique.VerdictProvider = (*CritiqueVerdictProvider)(nil)

// NewCritiqueVerdictProvider returns a provider that reads through r.
func NewCritiqueVerdictProvider(r analysisResultReader) *CritiqueVerdictProvider {
	return &CritiqueVerdictProvider{results: r}
}

// Verdicts returns the job's proposed verdicts: relationship results first,
// then requires results, each in stored order. A relationship result typed
// NO_RELATIONSHIP is not a verdict to review and is skipped. tenantID must be
// the tenant in ctx, which scopes the read.
func (p *CritiqueVerdictProvider) Verdicts(ctx context.Context, tenantID, jobID string) ([]critique.ProposedVerdict, error) {
	ctxTenant, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("critique verdicts: %w", err)
	}
	if tenantID != ctxTenant {
		return nil, fmt.Errorf("critique verdicts: asked for tenant %q in a context for another tenant: %w", tenantID, tenant.ErrTenantMismatch)
	}
	stored, err := p.results.GetCompletedAnalysisResults(ctx, jobID,
		[]string{results.RelationshipAnalyzer, results.RequiresAnalyzer})
	if err != nil {
		return nil, fmt.Errorf("critique verdicts: reading the relationship and requires results of job %s: %w", jobID, err)
	}

	var verdicts []critique.ProposedVerdict
	var matches []results.SemanticMatchResult
	if err := decodeStored(stored, results.RelationshipAnalyzer, jobID, &matches); err != nil {
		return nil, err
	}
	for _, m := range matches {
		if m.RelationshipType == relationship.RelNoRelationship.String() {
			continue
		}
		verdicts = append(verdicts, critique.ProposedVerdict{Kind: critique.KindRelationship,
			SourceControlID: m.SourceID, TargetControlID: m.TargetID, Verdict: m.RelationshipType, Justifications: m.Justifications})
	}
	var prerequisites []results.RequiresResult
	if err := decodeStored(stored, results.RequiresAnalyzer, jobID, &prerequisites); err != nil {
		return nil, err
	}
	for _, r := range prerequisites {
		verdicts = append(verdicts, critique.ProposedVerdict{Kind: critique.KindRequires,
			SourceControlID: r.SourceID, TargetControlID: r.TargetID, Verdict: requiresVerdict, Justifications: r.Justifications})
	}
	return verdicts, nil
}

// decodeStored decodes analyzer name's stored result into v. A missing
// result (the analyzer is disabled) leaves v unchanged.
func decodeStored(stored map[string]*analyzer.Output, name, jobID string, v any) error {
	out, ok := stored[name]
	if !ok || out == nil || len(out.ResultData) == 0 {
		return nil
	}
	if err := json.Unmarshal(out.ResultData, v); err != nil {
		return fmt.Errorf("critique verdicts: the stored %s result of job %s does not decode, so it is corrupt; re-run the job: %w",
			name, jobID, err)
	}
	return nil
}

// critiqueDecisions returns the relationship and requires decisions of a job's
// stored critique result in outputs. A job without one (it was created
// without a critique stage) gets decisions that veto nothing, so its pairs
// pass through unchanged. Applying the decisions is pure, so a resumed job
// that reloads the same critique result applies the same vetoes.
func critiqueDecisions(outputs map[string]*analyzer.Output) (rel, req results.Verdicts, err error) {
	var crit []results.CritiqueResult
	if out, ok := outputs[results.CritiqueAnalyzer]; ok && out != nil && len(out.ResultData) > 0 {
		if err := json.Unmarshal(out.ResultData, &crit); err != nil {
			return results.Verdicts{}, results.Verdicts{}, fmt.Errorf("the stored critique result does not decode, so it is corrupt; "+
				"create a new job to re-run the analysis instead of resuming this one: %w", err)
		}
	}
	return results.Apply(results.RelationshipAnalyzer, crit), results.Apply(results.RequiresAnalyzer, crit), nil
}
