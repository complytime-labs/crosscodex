package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/complytime-labs/crosscodex/internal/analyzer/relationship"
	"github.com/complytime-labs/crosscodex/internal/analyzer/requires"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

// ErrNotACatalogJob indicates a job's config has no catalog_id (created from
// document_content or document_uri instead) — candidate generation is a
// legitimate no-op for such jobs, not an error. Callers treat this as "skip
// candidate generation, proceed to the next pass."
var ErrNotACatalogJob = errors.New("job is not catalog-backed")

// ErrInsufficientEmbeddingCoverage indicates fewer of the job's controls have
// an embeddings row than CandidateConfig.MinEmbeddingCoverage requires. This
// fails the stage loudly rather than silently producing near-empty candidates.
var ErrInsufficientEmbeddingCoverage = errors.New("insufficient embedding coverage for candidate generation")

// ExtractCatalogID reads catalog_id from a job's raw JSON config. jobConfig is
// the same []byte the executor unmarshals into a generic map: pb.JobConfig has
// no json struct tags, so encoding/json marshals the catalog_id oneof variant
// under the literal Go field names jobConfig["Source"]["CatalogId"]. Returns
// ok=false (not an error) for document_content/document_uri jobs and for
// malformed or empty config.
func ExtractCatalogID(jobConfig []byte) (catalogID string, ok bool) {
	var raw map[string]interface{}
	if err := json.Unmarshal(jobConfig, &raw); err != nil {
		return "", false
	}
	source, ok := raw["Source"].(map[string]interface{})
	if !ok {
		return "", false
	}
	id, ok := source["CatalogId"].(string)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// ControlsReader reads a catalog's non-section controls, enriched with
// classification (type/level) read from the classify analyzer's persisted
// analysis_results blob for the given job.
type ControlsReader interface {
	ControlData(ctx context.Context, tenantID, catalogID, jobID string) (map[string]*candidate.ControlData, error)
}

// EmbeddingsReader reads a catalog's embedding vectors for one model and
// returns the full pairwise cosine-similarity matrix, shaped for
// candidate.GenerateRequest.EmbeddingMatrix. Similarity values are on the
// [0, 100] scale the candidate generators expect.
type EmbeddingsReader interface {
	SimilarityMatrix(ctx context.Context, tenantID, catalogID, model string) (ids []string, values [][]float32, err error)
}

// CandidateWriter persists generated candidates for both consumers.
type CandidateWriter interface {
	WriteRequires(ctx context.Context, tenantID, jobID string, pairs []requires.RequiresPair) error
	WriteRelationship(ctx context.Context, tenantID, jobID string, pairs []relationship.CandidatePair, provenance map[string][]byte) error
}

// CandidateGenerator orchestrates candidate generation for a catalog-backed
// job: it reads the job's controls (enriched with classify results) and the
// catalog's embeddings, verifies embedding coverage, runs the candidate
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

// Generate runs candidate generation for a single job. It returns
// ErrNotACatalogJob when the job has no catalog_id (a legitimate skip) and
// ErrInsufficientEmbeddingCoverage when too few controls are embedded.
func (g *CandidateGenerator) Generate(ctx context.Context, tenantID, jobID string, jobConfig []byte) error {
	catalogID, ok := ExtractCatalogID(jobConfig)
	if !ok {
		return ErrNotACatalogJob
	}

	controlData, err := g.controls.ControlData(ctx, tenantID, catalogID, jobID)
	if err != nil {
		return fmt.Errorf("candidate generation: reading controls: %w", err)
	}

	ids, values, err := g.embeddings.SimilarityMatrix(ctx, tenantID, catalogID, g.embedModel)
	if err != nil {
		return fmt.Errorf("candidate generation: reading embeddings: %w", err)
	}

	// Coverage: fraction of (non-section) controls that have at least one
	// embeddings row. Skip the check entirely for a zero-control catalog — a
	// degenerate case, not a coverage failure, and it must not divide by zero.
	embedded := make(map[string]bool, len(ids))
	for _, id := range ids {
		embedded[id] = true
	}
	if len(controlData) > 0 {
		covered := 0
		for controlID := range controlData {
			if embedded[controlID] {
				covered++
			}
		}
		coverage := float64(covered) / float64(len(controlData))
		if coverage < g.cfg.MinEmbeddingCoverage {
			return fmt.Errorf("candidate generation: catalog %s coverage %.2f%% below minimum %.2f%%: %w",
				catalogID, coverage*100, g.cfg.MinEmbeddingCoverage*100, ErrInsufficientEmbeddingCoverage)
		}
	}

	// The candidate generators read IDs/Values from any struct via reflection
	// (pkg/analyzer/candidate/builtin/semantic.go's extractMatrixData).
	matrix := struct {
		IDs    []string
		Values [][]float32
	}{IDs: ids, Values: values}

	req := candidate.GenerateRequest{
		TenantID:        tenantID,
		JobID:           jobID,
		SourceControls:  controlData,
		TargetControls:  controlData,
		EmbeddingMatrix: matrix,
	}
	candidates, err := g.registry.Generate(ctx, req, candidate.StrategyWeightedUnion)
	if err != nil {
		return fmt.Errorf("candidate generation: generating: %w", err)
	}

	// Both consumers receive the same candidate list, converted to their
	// respective pair shapes plus provenance.
	requiresPairs := make([]requires.RequiresPair, len(candidates))
	relationshipPairs := make([]relationship.CandidatePair, len(candidates))
	provenance := make(map[string][]byte, len(candidates))
	for i, c := range candidates {
		prov := []requires.CandidateProvenance{{
			GeneratorName: c.GeneratorID,
			Score:         c.Score,
			Weight:        c.Weight,
			Metadata:      c.Metadata,
		}}
		requiresPairs[i] = requires.RequiresPair{
			SourceControlID: c.SourceID,
			TargetControlID: c.TargetID,
			AggregateScore:  c.Score,
			Provenance:      prov,
		}
		relationshipPairs[i] = relationship.CandidatePair{
			SourceControlID: c.SourceID,
			TargetControlID: c.TargetID,
			// candidate.Candidate.Score is [0,1]; CandidatePair.SimilarityScore's
			// contract is [0,100] (relationship/candidates.go), so rescale to
			// match the scale the direct-embedding relationship path produces.
			SimilarityScore: float32(c.Score * 100),
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
