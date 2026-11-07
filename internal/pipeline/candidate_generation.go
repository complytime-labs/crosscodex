package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/internal/analyzer/relationship"
	"github.com/complytime-labs/crosscodex/internal/analyzer/requires"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/jobconfig"
)

// ErrMissingCatalogID indicates a job's stored config has no usable catalog
// selection: catalog_id is missing, empty or not a string, or a catalog field
// or bidirectional has the wrong type (see jobconfig.ExtractCatalogs).
// CreateJob refuses such configs, so the row predates import-only analysis
// or was written outside CreateJob. The job fails instead of analysing zero
// controls. Its errors are tenantErrors: their text reaches the tenant.
var ErrMissingCatalogID = errors.New("job config has no usable catalog_id (missing, empty, or a catalog field of the wrong type)")

// ErrInsufficientEmbeddingCoverage indicates fewer of the job's controls have
// an embeddings row than CandidateConfig.MinEmbeddingCoverage requires. This
// fails the stage loudly rather than silently producing near-empty candidates.
var ErrInsufficientEmbeddingCoverage = errors.New("insufficient embedding coverage for candidate generation")

// ControlsReader reads a catalog's non-section controls, enriched with
// classification (type/level) read from the classify analyzer's persisted
// analysis_results blob for the given job.
type ControlsReader interface {
	ControlData(ctx context.Context, tenantID, catalogID, jobID string) (map[string]*candidate.ControlData, error)
}

// EmbeddingsReader reads the embedding vectors of a job's catalogs for one
// model and returns the full pairwise cosine-similarity matrix over all of
// them, shaped for candidate.GenerateRequest.EmbeddingMatrix. Similarity
// values are on the [0, 100] scale the candidate generators expect.
type EmbeddingsReader interface {
	SimilarityMatrix(ctx context.Context, tenantID string, catalogIDs []string, model string) (ids []string, values [][]float32, err error)
}

// CandidateWriter persists generated candidates for both consumers.
type CandidateWriter interface {
	WriteRequires(ctx context.Context, tenantID, jobID string, pairs []requires.RequiresPair) error
	WriteRelationship(ctx context.Context, tenantID, jobID string, pairs []relationship.CandidatePair, provenance map[string][]byte) error
}

// CandidateGenerator orchestrates candidate generation for a job's catalogs:
// it reads the job's controls (enriched with classify results) and the
// catalogs' embeddings, verifies embedding coverage, runs the candidate
// registry, and persists results for both the requires and relationship
// analyzers.
type CandidateGenerator struct {
	controls   ControlsReader
	embeddings EmbeddingsReader
	registry   *candidate.Registry
	writer     CandidateWriter
	cfg        config.CandidateConfig
	embedModel string
}

// NewCandidateGenerator constructs a CandidateGenerator. embedModel is the
// embeddings model to read; the caller (the executor) decides which configured
// model to use, keeping this component free of embedding-config knowledge.
func NewCandidateGenerator(controls ControlsReader, embeddings EmbeddingsReader, registry *candidate.Registry, writer CandidateWriter, cfg config.CandidateConfig, embedModel string) *CandidateGenerator {
	return &CandidateGenerator{
		controls:   controls,
		embeddings: embeddings,
		registry:   registry,
		writer:     writer,
		cfg:        cfg,
		embedModel: embedModel,
	}
}

// Generate runs candidate generation over a job's catalogs. It returns
// ErrMissingCatalogID when the job config has no catalog_id and
// ErrInsufficientEmbeddingCoverage when too few of either catalog's
// controls are embedded.
func (g *CandidateGenerator) Generate(ctx context.Context, tenantID, jobID string, jobConfig []byte) error {
	cats, ok := jobconfig.ExtractCatalogs(jobConfig)
	if !ok {
		return &tenantError{sentinel: ErrMissingCatalogID, message: fmt.Sprintf("candidate generation: %v; start a new analysis: %s",
			ErrMissingCatalogID, newJobCommand(&Job{JobID: jobID, Config: jobConfig}))}
	}

	source, skipped, err := g.controlsWithText(ctx, tenantID, cats.SourceID, jobID)
	if err != nil {
		return err
	}
	target := source
	if cats.CrossCatalog() {
		var targetSkipped int
		target, targetSkipped, err = g.controlsWithText(ctx, tenantID, cats.TargetID, jobID)
		if err != nil {
			return err
		}
		skipped += targetSkipped
	}
	// If every control is blank the job succeeds with no candidates and the
	// pair stages produce nothing; record the count on the active span so
	// that outcome is visible.
	if skipped > 0 {
		trace.SpanFromContext(ctx).SetAttributes(attribute.Int("candidates.skipped_blank_statement", skipped))
	}

	ids, values, err := g.embeddings.SimilarityMatrix(ctx, tenantID, cats.CatalogIDs(), g.embedModel)
	if err != nil {
		return fmt.Errorf("candidate generation: reading embeddings: %w", err)
	}
	embedded := make(map[string]bool, len(ids))
	for _, id := range ids {
		embedded[id] = true
	}
	if !cats.CrossCatalog() {
		if err := g.checkCoverage("catalog "+cats.SourceID, source, embedded); err != nil {
			return err
		}
	} else {
		if err := g.checkCoverage("source catalog "+cats.SourceID, source, embedded); err != nil {
			return err
		}
		if err := g.checkCoverage("target catalog "+cats.TargetID, target, embedded); err != nil {
			return err
		}
	}

	// The candidate generators read IDs/Values from any struct via reflection
	// (pkg/analyzer/candidate/builtin/semantic.go's extractMatrixData).
	rowOf := make(map[string]int, len(ids))
	for i, id := range ids {
		rowOf[id] = i
	}
	matrix := struct {
		IDs    []string
		Values [][]float32
	}{IDs: ids, Values: values}

	// Pairs run source to target. Within one catalog source and target are
	// the same control set, so the generators pair its controls in both
	// directions. bidirectional adds target to source across catalogs.
	requests := []candidate.GenerateRequest{{
		TenantID: tenantID, JobID: jobID, SourceControls: source, TargetControls: target, EmbeddingMatrix: matrix,
	}}
	if cats.Bidirectional {
		requests = append(requests, candidate.GenerateRequest{
			TenantID: tenantID, JobID: jobID, SourceControls: target, TargetControls: source, EmbeddingMatrix: matrix,
		})
	}
	var candidates []candidate.Candidate
	for _, req := range requests {
		got, err := g.registry.Generate(ctx, req, candidate.StrategyRankFusion, candidate.WithMaxPerSource(g.cfg.MaxPerSource))
		if err != nil {
			return fmt.Errorf("candidate generation: generating: %w", err)
		}
		candidates = append(candidates, got...)
	}

	// Both consumers receive the same candidate list, converted to their
	// respective pair shapes plus provenance.
	requiresPairs := make([]requires.RequiresPair, len(candidates))
	relationshipPairs := make([]relationship.CandidatePair, len(candidates))
	provenance := make(map[string][]byte, len(candidates))
	for i, c := range candidates {
		prov := make([]requires.CandidateProvenance, len(c.Contributions))
		for j, contribution := range c.Contributions {
			metadata := maps.Clone(contribution.Metadata)
			if metadata == nil {
				metadata = make(map[string]string, 1)
			}
			metadata["rank"] = strconv.FormatFloat(contribution.Rank, 'g', -1, 64)
			prov[j] = requires.CandidateProvenance{
				GeneratorName: contribution.GeneratorID,
				Score:         contribution.Score,
				Weight:        contribution.Weight,
				Metadata:      metadata,
			}
		}
		requiresPairs[i] = requires.RequiresPair{
			SourceControlID: c.SourceID,
			TargetControlID: c.TargetID,
			AggregateScore:  c.Score,
			Provenance:      prov,
		}
		relationshipPairs[i] = relationship.CandidatePair{
			SourceControlID: c.SourceID,
			TargetControlID: c.TargetID,
			SimilarityScore: matrixSimilarity(rowOf, values, c.SourceID, c.TargetID),
		}
		provJSON, err := json.Marshal(prov)
		if err != nil {
			return fmt.Errorf("candidate generation: marshaling provenance for %s--%s: %w", c.SourceID, c.TargetID, err)
		}
		provenance[c.SourceID+"--"+c.TargetID] = provJSON
	}

	if err := g.writer.WriteRequires(ctx, tenantID, jobID, requiresPairs); err != nil {
		return fmt.Errorf("candidate generation: writing requires candidates: %w", err)
	}
	if err := g.writer.WriteRelationship(ctx, tenantID, jobID, relationshipPairs, provenance); err != nil {
		return fmt.Errorf("candidate generation: writing relationship candidates: %w", err)
	}
	return nil
}

// matrixSimilarity returns the pair's value in the [0, 100] similarity matrix,
// or 0 when either control has no embedding row (a pair a non-embedding
// generator proposed).
func matrixSimilarity(rowOf map[string]int, values [][]float32, sourceID, targetID string) float32 {
	i, sourceOK := rowOf[sourceID]
	j, targetOK := rowOf[targetID]
	if !sourceOK || !targetOK {
		return 0
	}
	return values[i][j]
}

// controlsWithText reads a catalog's controls and drops those without a
// statement: such a control has no requirement text to compare, so a pair
// containing it would fail the pair stages (they refuse blank prompts), and
// its embedding (the ancestor title only, if any) says nothing about the
// control. Leaving it out also keeps it from understating embedding
// coverage. It returns how many controls it dropped.
func (g *CandidateGenerator) controlsWithText(ctx context.Context, tenantID, catalogID, jobID string) (map[string]*candidate.ControlData, int, error) {
	all, err := g.controls.ControlData(ctx, tenantID, catalogID, jobID)
	if err != nil {
		return nil, 0, fmt.Errorf("candidate generation: reading controls of catalog %s: %w", catalogID, err)
	}
	withText := make(map[string]*candidate.ControlData, len(all))
	for id, c := range all {
		if strings.TrimSpace(c.Text) != "" {
			withText[id] = c
		}
	}
	return withText, len(all) - len(withText), nil
}

// checkCoverage fails when fewer of side's controls have an embedding than
// CandidateConfig.MinEmbeddingCoverage requires. side names the catalog for
// the error. A side with no controls is a degenerate case, not a coverage
// failure, and must not divide by zero.
func (g *CandidateGenerator) checkCoverage(side string, controls map[string]*candidate.ControlData, embedded map[string]bool) error {
	if len(controls) == 0 {
		return nil
	}
	covered := 0
	for controlID := range controls {
		if embedded[controlID] {
			covered++
		}
	}
	coverage := float64(covered) / float64(len(controls))
	if coverage < g.cfg.MinEmbeddingCoverage {
		return fmt.Errorf("candidate generation: %s: %d of %d controls with a statement (%.2f%%) have a %s embedding, "+
			"below analysis.candidates.min_embedding_coverage (%.2f%%): %w. "+
			"Once the embedding stage succeeds for those controls, resume the job: crosscodex run retry --from-failure <job-id>",
			side, covered, len(controls), coverage*100, g.embedModel, g.cfg.MinEmbeddingCoverage*100, ErrInsufficientEmbeddingCoverage)
	}
	return nil
}
