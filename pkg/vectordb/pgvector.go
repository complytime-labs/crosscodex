package vectordb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// PgVectorStore implements both Index and VectorDB interfaces using pgvector
type PgVectorStore struct {
	db     *sql.DB
	tracer trace.Tracer
	meter  metric.Meter

	// Metrics
	searchCounter metric.Int64Counter
	searchLatency metric.Int64Histogram
	storeCounter  metric.Int64Counter
	storeLatency  metric.Int64Histogram
}

// Option configures PgVectorStore construction.
type Option func(*PgVectorStore) error

// WithTelemetry configures OpenTelemetry tracing and metrics for the vector store.
// Initializes counters and histograms for search and store operations.
func WithTelemetry(tracer trace.Tracer, meter metric.Meter) Option {
	return func(store *PgVectorStore) error {
		store.tracer = tracer
		store.meter = meter

		var err error
		store.searchCounter, err = meter.Int64Counter(
			"vectordb.searches.total",
			metric.WithDescription("Total number of similarity searches performed"),
		)
		if err != nil {
			return fmt.Errorf("failed to create search counter: %w", err)
		}

		store.searchLatency, err = meter.Int64Histogram(
			"vectordb.search.duration_ms",
			metric.WithDescription("Duration of similarity searches in milliseconds"),
		)
		if err != nil {
			return fmt.Errorf("failed to create search latency histogram: %w", err)
		}

		store.storeCounter, err = meter.Int64Counter(
			"vectordb.embeddings.stored.total",
			metric.WithDescription("Total number of embeddings stored"),
		)
		if err != nil {
			return fmt.Errorf("failed to create store counter: %w", err)
		}

		store.storeLatency, err = meter.Int64Histogram(
			"vectordb.store.duration_ms",
			metric.WithDescription("Duration of embedding store operations in milliseconds"),
		)
		if err != nil {
			return fmt.Errorf("failed to create store latency histogram: %w", err)
		}

		return nil
	}
}

// NewPgVectorStore creates a new PostgreSQL vector store.
// Options are applied in order after the base store is constructed.
func NewPgVectorStore(db *sql.DB, opts ...Option) (*PgVectorStore, error) {
	if db == nil {
		return nil, fmt.Errorf("database connection is required")
	}

	store := &PgVectorStore{
		db: db,
	}

	for _, opt := range opts {
		if err := opt(store); err != nil {
			return nil, fmt.Errorf("failed to apply option: %w", err)
		}
	}

	return store, nil
}

// Index interface implementation — delegates to domain-specific VectorDB
// methods where possible, uses direct SQL for operations without a
// domain-specific equivalent (Delete, Get, Count).

// Insert adds or updates a vector embedding.
// Extracts catalog_id and model from metadata, defaulting to "generic" if absent.
// Delegates to StoreEmbedding for the actual database operation.
func (db *PgVectorStore) Insert(ctx context.Context, id string, vector []float32, metadata map[string]any) error {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return fmt.Errorf("tenant context required: %w", err)
	}

	catalogID := "generic"
	model := "generic"
	if metadata != nil {
		if v, ok := metadata["catalog_id"].(string); ok && v != "" {
			catalogID = v
		}
		if v, ok := metadata["model"].(string); ok && v != "" {
			model = v
		}
	}

	embedding := Embedding{
		CatalogID: catalogID,
		ControlID: id,
		Model:     model,
		Vector:    vector,
		Metadata:  metadata,
	}

	return db.StoreEmbedding(ctx, tenantID, embedding)
}

// Search finds the k-nearest neighbors to the query vector.
// Delegates to FindSimilar with generic catalog and model defaults,
// then converts SimilarityResult to Match.
func (db *PgVectorStore) Search(ctx context.Context, query []float32, limit int) ([]Match, error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenant context required: %w", err)
	}

	fsq := FindSimilarQuery{
		CatalogID: "generic",
		Model:     "generic",
		Vector:    query,
		Limit:     limit,
	}

	results, err := db.FindSimilar(ctx, tenantID, fsq)
	if err != nil {
		return nil, err
	}

	matches := make([]Match, len(results))
	for i, r := range results {
		matches[i] = Match{
			ID:       r.ControlID,
			Score:    r.Similarity,
			Metadata: r.Metadata,
		}
	}
	return matches, nil
}

// Delete removes vector embeddings by ID (control_id).
// Uses direct SQL since there is no domain-specific equivalent.
// Deletes all embeddings for the given control ID across all catalogs and models.
// Returns ErrNotFound if no row matches the given ID and tenant.
func (db *PgVectorStore) Delete(ctx context.Context, id string) error {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return fmt.Errorf("tenant context required: %w", err)
	}

	result, err := db.db.ExecContext(ctx,
		`DELETE FROM embeddings WHERE tenant_id = $1 AND control_id = $2`,
		tenantID, id)
	if err != nil {
		return fmt.Errorf("failed to delete embedding: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return ErrNotFound
	}

	return nil
}

// Get retrieves a vector embedding by ID (control_id).
// Uses direct SQL since there is no domain-specific equivalent.
// If multiple embeddings exist for the same control ID (different catalogs/models),
// returns an arbitrary one. Returns ErrNotFound if the embedding does not exist.
func (db *PgVectorStore) Get(ctx context.Context, id string) (*Match, error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenant context required: %w", err)
	}

	var controlID, vectorStr string
	err = db.db.QueryRowContext(ctx,
		`SELECT control_id, vector FROM embeddings WHERE tenant_id = $1 AND control_id = $2 LIMIT 1`,
		tenantID, id).Scan(&controlID, &vectorStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to get embedding: %w", err)
	}

	vector, err := parseVectorString(vectorStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse vector: %w", err)
	}

	return &Match{
		ID:       controlID,
		Score:    1.0,
		Vector:   vector,
		Metadata: make(map[string]any),
	}, nil
}

// Count returns the total number of embeddings for the current tenant.
func (db *PgVectorStore) Count(ctx context.Context) (int64, error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("tenant context required: %w", err)
	}

	var count int64
	err = db.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE tenant_id = $1`,
		tenantID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count embeddings: %w", err)
	}

	return count, nil
}

// VectorDB interface implementation - minimal stubs for now

// StoreEmbedding adds or updates a single embedding with compliance metadata
func (db *PgVectorStore) StoreEmbedding(ctx context.Context, tenantID string, embedding Embedding) error {
	ctx, span := db.startSpan(ctx, "vectordb.store_embedding")
	defer span.End()

	// Add telemetry attributes
	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.String("catalog.id", embedding.CatalogID),
		attribute.String("control.id", embedding.ControlID),
		attribute.String("model", embedding.Model),
		attribute.Int("vector.dimensions", len(embedding.Vector)),
	)

	if err := validateTenantContext(ctx, span, tenantID); err != nil {
		return err
	}

	// Start timing
	start := time.Now()
	defer func() {
		if db.storeLatency != nil {
			db.storeLatency.Record(ctx, time.Since(start).Milliseconds())
		}
	}()

	// Increment counter
	if db.storeCounter != nil {
		db.storeCounter.Add(ctx, 1)
	}

	// Convert vector to pgvector format (placeholder - will implement SQL in next step)
	query := `
        INSERT INTO embeddings (catalog_id, control_id, model, vector, tenant_id)
        VALUES ($1, $2, $3, $4, $5)
        ON CONFLICT (catalog_id, control_id, model) 
        DO UPDATE SET vector = EXCLUDED.vector, tenant_id = EXCLUDED.tenant_id`

	_, err := db.db.ExecContext(ctx, query,
		embedding.CatalogID,
		embedding.ControlID,
		embedding.Model,
		vectorToString(embedding.Vector),
		tenantID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("failed to store embedding: %w", err)
	}

	span.SetStatus(codes.Ok, "embedding stored successfully")
	return nil
}

// vectorToString converts float32 slice to pgvector format string
func vectorToString(vector []float32) string {
	if len(vector) == 0 {
		return "[]"
	}

	var b strings.Builder
	b.WriteByte('[')
	for i, v := range vector {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%g", v)
	}
	b.WriteByte(']')
	return b.String()
}

// StoreBatch efficiently stores multiple embeddings in a single transaction.
// An empty batch is a no-op that returns nil without touching the database.
func (db *PgVectorStore) StoreBatch(ctx context.Context, tenantID string, embeddings []Embedding) error {
	ctx, span := db.startSpan(ctx, "vectordb.store_batch")
	defer span.End()

	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.Int("batch.size", len(embeddings)),
	)

	if err := validateTenantContext(ctx, span, tenantID); err != nil {
		return err
	}

	if len(embeddings) == 0 {
		span.SetStatus(codes.Ok, "empty batch")
		return nil
	}

	// Start timing
	start := time.Now()
	defer func() {
		if db.storeLatency != nil {
			db.storeLatency.Record(ctx, time.Since(start).Milliseconds())
		}
	}()

	// Use a transaction so the batch is atomic — all embeddings
	// succeed or none do, preventing partial writes.
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	// Prepare the upsert statement once and reuse it for each row,
	// avoiding repeated query parsing overhead on large batches.
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO embeddings (catalog_id, control_id, model, vector, tenant_id)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (catalog_id, control_id, model)
		DO UPDATE SET vector = EXCLUDED.vector, tenant_id = EXCLUDED.tenant_id`)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("failed to prepare statement: %w", err)
	}
	defer stmt.Close()

	for i, embedding := range embeddings {
		_, err = stmt.ExecContext(ctx,
			embedding.CatalogID,
			embedding.ControlID,
			embedding.Model,
			vectorToString(embedding.Vector),
			tenantID)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			return fmt.Errorf("failed to store embedding %d: %w", i, err)
		}
	}

	if err = tx.Commit(); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("failed to commit batch: %w", err)
	}

	if db.storeCounter != nil {
		db.storeCounter.Add(ctx, int64(len(embeddings)))
	}

	span.SetStatus(codes.Ok, "batch stored successfully")
	return nil
}

// FindSimilar searches for embeddings similar to the query vector.
// Only searches embeddings from the specified model to ensure vector compatibility.
// Returns ErrModelNotFound if no embeddings exist for the given model and catalog.
func (db *PgVectorStore) FindSimilar(ctx context.Context, tenantID string, query FindSimilarQuery) ([]SimilarityResult, error) {
	ctx, span := db.startSpan(ctx, "vectordb.find_similar")
	defer span.End()

	// Add telemetry attributes
	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.String("catalog.id", query.CatalogID),
		attribute.String("model", query.Model),
		attribute.Int("limit", query.Limit),
		attribute.Int("query.dimensions", len(query.Vector)),
	)

	if err := validateTenantContext(ctx, span, tenantID); err != nil {
		return nil, err
	}

	// Validate query parameters
	if query.Limit <= 0 {
		span.SetStatus(codes.Error, "invalid limit")
		return nil, fmt.Errorf("limit must be positive, got %d", query.Limit)
	}
	if len(query.Vector) == 0 {
		span.SetStatus(codes.Error, "empty vector")
		return nil, fmt.Errorf("query vector cannot be empty")
	}

	// Start timing
	start := time.Now()
	defer func() {
		if db.searchLatency != nil {
			db.searchLatency.Record(ctx, time.Since(start).Milliseconds())
		}
	}()

	// Increment search counter
	if db.searchCounter != nil {
		db.searchCounter.Add(ctx, 1)
	}

	// Check model existence separately from the similarity query.
	// This distinguishes "model doesn't exist" (ErrModelNotFound) from
	// "model exists but no similar results" (empty slice), giving callers
	// actionable error information. The extra round-trip is acceptable
	// because similarity searches are not latency-critical hot paths.
	var count int
	checkQuery := `SELECT COUNT(*) FROM embeddings WHERE tenant_id = $1 AND catalog_id = $2 AND model = $3`
	err := db.db.QueryRowContext(ctx, checkQuery, tenantID, query.CatalogID, query.Model).Scan(&count)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("failed to check model existence: %w", err)
	}
	if count == 0 {
		span.SetStatus(codes.Error, "model not found")
		return nil, ErrModelNotFound
	}

	// Execute similarity search using pgvector cosine distance operator (<=>)
	// Cosine similarity = 1 - cosine distance
	sqlQuery := `
		SELECT control_id, 1 - (vector <=> $1) as similarity
		FROM embeddings
		WHERE tenant_id = $2 AND catalog_id = $3 AND model = $4
		ORDER BY vector <=> $1
		LIMIT $5`

	rows, err := db.db.QueryContext(ctx, sqlQuery,
		vectorToString(query.Vector),
		tenantID,
		query.CatalogID,
		query.Model,
		query.Limit)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("similarity search failed: %w", err)
	}
	defer rows.Close()

	var results []SimilarityResult
	for rows.Next() {
		var result SimilarityResult
		if err := rows.Scan(&result.ControlID, &result.Similarity); err != nil {
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("failed to scan result: %w", err)
		}
		result.Metadata = make(map[string]any)
		results = append(results, result)
	}

	if err = rows.Err(); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("error iterating results: %w", err)
	}

	span.SetAttributes(attribute.Int("results.count", len(results)))
	span.SetStatus(codes.Ok, "similarity search completed")
	return results, nil
}

// DeleteByModel removes all embeddings for a specific catalog and model.
// This is used when reprocessing after switching embedding models.
func (db *PgVectorStore) DeleteByModel(ctx context.Context, tenantID, catalogID, model string) error {
	ctx, span := db.startSpan(ctx, "vectordb.delete_by_model")
	defer span.End()

	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.String("catalog.id", catalogID),
		attribute.String("model", model),
	)

	if err := validateTenantContext(ctx, span, tenantID); err != nil {
		return err
	}

	query := `DELETE FROM embeddings WHERE tenant_id = $1 AND catalog_id = $2 AND model = $3`
	result, err := db.db.ExecContext(ctx, query, tenantID, catalogID, model)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("failed to delete embeddings: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	span.SetAttributes(attribute.Int64("rows.deleted", rowsAffected))
	span.SetStatus(codes.Ok, "embeddings deleted successfully")
	return nil
}

// parseVectorString parses a pgvector format string like "[1.0,2.0,3.0]" into []float32.
// Returns an error if the string is malformed or contains non-numeric values.
func parseVectorString(s string) ([]float32, error) {
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil, fmt.Errorf("invalid vector format: must be enclosed in brackets, got %q", s)
	}

	inner := s[1 : len(s)-1]
	if inner == "" {
		return nil, nil
	}

	parts := strings.Split(inner, ",")
	result := make([]float32, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		f, err := strconv.ParseFloat(p, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid vector element %q: %w", p, err)
		}
		result = append(result, float32(f))
	}

	return result, nil
}

// startSpan begins a new trace span using the store's configured tracer,
// falling back to the context's tracer provider when no explicit tracer is set.
func (db *PgVectorStore) startSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	if db.tracer != nil {
		return db.tracer.Start(ctx, name)
	}
	return trace.SpanFromContext(ctx).TracerProvider().Tracer("vectordb").Start(ctx, name)
}

// validateTenantContext checks that the context carries a tenant ID matching
// the explicit tenantID parameter. Returns a non-nil error (with span status
// set) when the context has no tenant or the IDs disagree.
func validateTenantContext(ctx context.Context, span trace.Span, tenantID string) error {
	contextTenant, err := tenant.FromContext(ctx)
	if err != nil {
		span.SetStatus(codes.Error, "tenant context missing")
		return fmt.Errorf("tenant context required: %w", err)
	}
	if contextTenant != tenantID {
		span.SetStatus(codes.Error, "tenant mismatch")
		return fmt.Errorf("tenant mismatch: context=%s, param=%s: %w", contextTenant, tenantID, tenant.ErrTenantMismatch)
	}
	return nil
}

// Verify interface compliance at compile time
var _ Index = (*PgVectorStore)(nil)
var _ VectorDB = (*PgVectorStore)(nil)
