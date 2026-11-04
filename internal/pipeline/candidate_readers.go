package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/internal/analyzer/relationship"
	"github.com/complytime-labs/crosscodex/internal/analyzer/requires"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// classifySectionClass mirrors the unexported sectionClass constant in
// internal/analyzer/classify (which matches oscal.ClassSection). Section
// controls are excluded from candidate generation, the same way classify skips
// them. Duplicated here because the source identifier is unexported.
const classifySectionClass = "compliance-section"

// pgControlsReader implements ControlsReader against the controls and
// analysis_results tables.
type pgControlsReader struct{ db db.TenantConnection }

// ControlData reads a catalog's non-section controls and merges in the
// classify analyzer's persisted type/level for the job. Controls without a
// classify entry keep empty Type/Level, which the level and keyword generators
// skip gracefully.
func (r *pgControlsReader) ControlData(ctx context.Context, tenantID, catalogID, jobID string) (map[string]*candidate.ControlData, error) {
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return nil, fmt.Errorf("pgControlsReader.ControlData: %w", err)
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("pgControlsReader.ControlData: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// title, statement, and parent_id are nullable; COALESCE keeps the scan
	// targets plain strings. class IS DISTINCT FROM handles a NULL class (a
	// control with no class is not a section and must be included). Withdrawn
	// controls (props.status) are excluded like sections; props may be NULL,
	// which COALESCE maps to "not withdrawn".
	rows, err := tx.Query(ctx, `
		SELECT control_id, COALESCE(title, ''), COALESCE(statement, ''), COALESCE(parent_id, '')
		FROM controls
		WHERE tenant_id = $1 AND catalog_id = $2 AND class IS DISTINCT FROM $3
		  AND COALESCE(props->>$4::text, '') <> $5
	`, tenantID, catalogID, classifySectionClass, oscal.PropStatus, oscal.PropStatusWithdrawn)
	if err != nil {
		return nil, fmt.Errorf("pgControlsReader.ControlData: querying controls: %w", err)
	}
	defer rows.Close()

	titleByID := make(map[string]string)
	type controlRow struct{ controlID, statement, parentID string }
	var controlRows []controlRow
	for rows.Next() {
		var controlID, title, statement, parentID string
		if err := rows.Scan(&controlID, &title, &statement, &parentID); err != nil {
			return nil, fmt.Errorf("pgControlsReader.ControlData: scanning control row: %w", err)
		}
		titleByID[controlID] = title
		controlRows = append(controlRows, controlRow{controlID, statement, parentID})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgControlsReader.ControlData: rows error: %w", err)
	}

	data := make(map[string]*candidate.ControlData, len(controlRows))
	for _, c := range controlRows {
		data[c.controlID] = &candidate.ControlData{
			ControlID: c.controlID,
			Text:      c.statement,
			Ancestor:  titleByID[c.parentID], // empty when no/unknown parent
		}
	}

	classifyRow := tx.QueryRow(ctx, `
		SELECT result_data FROM analysis_results
		WHERE tenant_id = $1 AND job_id = $2 AND analyzer_name = 'classify'
	`, tenantID, jobID)
	var classifyJSON []byte
	switch err := classifyRow.Scan(&classifyJSON); {
	case err == nil:
		var classified []results.ClassifyResult
		if err := json.Unmarshal(classifyJSON, &classified); err != nil {
			return nil, fmt.Errorf("pgControlsReader.ControlData: unmarshaling classify results: %w", err)
		}
		for _, c := range classified {
			if cd, ok := data[c.ControlID]; ok {
				cd.Type = c.Type
				cd.Level = c.Level
			}
		}
	case errors.Is(err, sql.ErrNoRows):
		// classify has not completed for this job yet (or produced nothing);
		// Type/Level stay empty and the level/keyword generators skip such
		// controls, so candidate generation degrades gracefully.
	default:
		return nil, fmt.Errorf("pgControlsReader.ControlData: reading classify results: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("pgControlsReader.ControlData: commit failed: %w", err)
	}
	return data, nil
}

// pgEmbeddingsReader implements EmbeddingsReader against the embeddings table.
type pgEmbeddingsReader struct {
	db     db.TenantConnection
	tracer trace.Tracer

	readCounter metric.Int64Counter
	readLatency metric.Float64Histogram
}

// PGEmbeddingsReaderOption configures the reader NewPGEmbeddingsReader builds.
type PGEmbeddingsReaderOption func(*pgEmbeddingsReader)

// WithPGEmbeddingsReaderTelemetry enables OTel tracing and metrics on the
// embeddings reader. crosscodexd passes the global providers, as it does for
// the pipeline store.
func WithPGEmbeddingsReaderTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) PGEmbeddingsReaderOption {
	return func(r *pgEmbeddingsReader) {
		if tp != nil {
			r.tracer = tp.Tracer("crosscodex/internal/pipeline")
		}
		if mp != nil {
			m := mp.Meter("crosscodex")
			r.readCounter, _ = m.Int64Counter("pipeline.embeddings.similarity_matrix.total",
				metric.WithDescription("Similarity matrix reads, by status"))
			r.readLatency, _ = m.Float64Histogram("pipeline.embeddings.similarity_matrix.duration_ms",
				metric.WithDescription("Similarity matrix read duration"))
		}
	}
}

// NewPGControlsReader constructs a ControlsReader backed by Postgres.
func NewPGControlsReader(conn db.TenantConnection) ControlsReader {
	return &pgControlsReader{db: conn}
}

// NewPGEmbeddingsReader constructs an EmbeddingsReader backed by Postgres.
func NewPGEmbeddingsReader(conn db.TenantConnection, opts ...PGEmbeddingsReaderOption) EmbeddingsReader {
	r := &pgEmbeddingsReader{db: conn}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// SimilarityMatrix builds the full pairwise cosine-similarity matrix over the
// embeddings of catalogIDs for one model via a single self-join using
// pgvector's <=> (cosine distance) operator: similarity = (1 - distance) *
// 100. The *100 scaling matches the [0, 100] convention the candidate
// generators expect (see internal/analyzer/embedding/similarity.go's
// buildSimilarityMatrix, where the semantic generator divides by 100). Both
// sides of the join are restricted to the tenant and to catalogIDs, so a job
// mapping one catalog onto another sees the similarities between them (and
// within each) but nothing from a third catalog or another tenant.
//
// The matrix is keyed by control ID alone. Control IDs are unique per tenant
// (controls' primary key is (tenant_id, control_id)), but embeddings are
// keyed by (tenant_id, catalog_id, control_id, model) with no foreign key to
// controls: when the controls upsert moves a control to another catalog, its
// embedding under the old catalog stays behind. Each side therefore joins
// controls on the control's current catalog, so a control contributes only
// the embedding filed under the catalog it belongs to now. A pair scanned
// twice anyway is an error, not a silent choice between two similarities.
// O(n^2) rows for n controls is acceptable at the catalog sizes this
// pipeline already processes with O(n) LLM calls per analyzer.
func (r *pgEmbeddingsReader) SimilarityMatrix(ctx context.Context, tenantID string, catalogIDs []string, model string) ([]string, [][]float32, error) {
	start := time.Now()
	ctx, span := telemetry.StartSpan(r.tracer, ctx, "pipeline.embeddings.similarity_matrix")
	defer span.End()
	span.SetAttributes(
		attribute.String("operation", "similarity_matrix"),
		attribute.StringSlice("catalog.ids", catalogIDs),
		attribute.String("embedding.model", model),
	)

	ids, values, err := r.similarityMatrix(ctx, span, tenantID, catalogIDs, model)
	status := "ok"
	if err != nil {
		status = "error"
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetAttributes(attribute.Int("controls.count", len(ids)))
		span.SetStatus(codes.Ok, "")
	}
	if r.readCounter != nil {
		r.readCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("operation", "similarity_matrix"), attribute.String("status", status)))
	}
	if r.readLatency != nil {
		r.readLatency.Record(ctx, float64(time.Since(start).Milliseconds()))
	}
	return ids, values, err
}

// similarityMatrix is SimilarityMatrix without its telemetry.
func (r *pgEmbeddingsReader) similarityMatrix(ctx context.Context, span trace.Span, tenantID string, catalogIDs []string, model string) (ids []string, values [][]float32, err error) {
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return nil, nil, fmt.Errorf("pgEmbeddingsReader.SimilarityMatrix: %w", err)
	}
	span.SetAttributes(attribute.String("tenant.id", tenantID))
	if len(catalogIDs) == 0 {
		return nil, nil, errors.New("pgEmbeddingsReader.SimilarityMatrix: no catalog IDs: a similarity matrix is read for a job's catalogs")
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("pgEmbeddingsReader.SimilarityMatrix: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.Query(ctx, `
		SELECT a.control_id, b.control_id, (1 - (a.vector <=> b.vector)) * 100
		FROM embeddings a
		JOIN controls ca
			ON ca.tenant_id = a.tenant_id AND ca.control_id = a.control_id AND ca.catalog_id = a.catalog_id
		JOIN embeddings b
			ON a.tenant_id = b.tenant_id AND a.model = b.model
		JOIN controls cb
			ON cb.tenant_id = b.tenant_id AND cb.control_id = b.control_id AND cb.catalog_id = b.catalog_id
		WHERE a.tenant_id = $1 AND b.tenant_id = $1
			AND a.catalog_id = ANY($2::text[]) AND b.catalog_id = ANY($2::text[])
			AND a.model = $3
	`, tenantID, catalogIDs, model)
	if err != nil {
		return nil, nil, fmt.Errorf("pgEmbeddingsReader.SimilarityMatrix: querying embeddings: %w", err)
	}
	defer rows.Close()

	type simRow struct {
		sourceID   string
		targetID   string
		similarity float32
	}
	seen := make(map[string]struct{})
	seenPairs := make(map[[2]string]struct{})
	var simRows []simRow
	for rows.Next() {
		var sourceID, targetID string
		var similarity float64
		if err := rows.Scan(&sourceID, &targetID, &similarity); err != nil {
			return nil, nil, fmt.Errorf("pgEmbeddingsReader.SimilarityMatrix: scanning row: %w", err)
		}
		pair := [2]string{sourceID, targetID}
		if _, dup := seenPairs[pair]; dup {
			return nil, nil, fmt.Errorf("pgEmbeddingsReader.SimilarityMatrix: the pair %s -> %s was read more than once for model %s: "+
				"each control must have one embedding under its current catalog", sourceID, targetID, model)
		}
		seenPairs[pair] = struct{}{}
		if _, ok := seen[sourceID]; !ok {
			seen[sourceID] = struct{}{}
			ids = append(ids, sourceID)
		}
		simRows = append(simRows, simRow{sourceID, targetID, float32(similarity)})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("pgEmbeddingsReader.SimilarityMatrix: rows error: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("pgEmbeddingsReader.SimilarityMatrix: commit failed: %w", err)
	}

	// Stable ordering so Values row/column indices are deterministic.
	sort.Strings(ids)
	index := make(map[string]int, len(ids))
	for i, id := range ids {
		index[id] = i
	}
	values = make([][]float32, len(ids))
	for i := range values {
		values[i] = make([]float32, len(ids))
	}
	for _, s := range simRows {
		si, sok := index[s.sourceID]
		ti, tok := index[s.targetID]
		if sok && tok {
			values[si][ti] = s.similarity
		}
	}
	return ids, values, nil
}

// pgCandidateWriter implements CandidateWriter by delegating to the requires
// and relationship candidate writers (Tasks 11/12) rather than reimplementing
// the upsert SQL.
type pgCandidateWriter struct{ db db.TenantConnection }

// NewPGCandidateWriter constructs a CandidateWriter backed by Postgres.
func NewPGCandidateWriter(conn db.TenantConnection) CandidateWriter {
	return &pgCandidateWriter{db: conn}
}

func (w *pgCandidateWriter) WriteRequires(ctx context.Context, tenantID, jobID string, pairs []requires.RequiresPair) error {
	return WriteRequiresCandidates(ctx, w.db, tenantID, jobID, pairs)
}

func (w *pgCandidateWriter) WriteRelationship(ctx context.Context, tenantID, jobID string, pairs []relationship.CandidatePair, provenance map[string][]byte) error {
	return WriteRelationshipCandidates(ctx, w.db, tenantID, jobID, pairs, provenance)
}
