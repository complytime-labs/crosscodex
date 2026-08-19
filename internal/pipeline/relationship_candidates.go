package pipeline

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/internal/analyzer/relationship"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// RelationshipCandidateProvider implements relationship.CandidateProvider by
// reading candidate pairs from the relationship_candidates table. This
// bridges candidate generation (which writes to the table) to the
// relationship analyzer (which needs pairs to classify). Mirrors
// RequiresCandidateProvider (requires_candidates.go).
type RelationshipCandidateProvider struct {
	db     db.TenantConnection
	tracer trace.Tracer

	// Metrics (optional, nil-safe)
	queryCounter   metric.Int64Counter
	queryLatency   metric.Int64Histogram
	candidateGauge metric.Int64Gauge
}

// Compile-time check that RelationshipCandidateProvider implements relationship.CandidateProvider.
var _ relationship.CandidateProvider = (*RelationshipCandidateProvider)(nil)

// NewRelationshipCandidateProvider creates a provider with the given database connection.
func NewRelationshipCandidateProvider(conn db.TenantConnection, opts ...RelationshipCandidateOption) *RelationshipCandidateProvider {
	p := &RelationshipCandidateProvider{
		db: conn,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// RelationshipCandidateOption configures the RelationshipCandidateProvider.
type RelationshipCandidateOption func(*RelationshipCandidateProvider)

// WithRelationshipCandidateTelemetry enables tracing and metrics for the provider.
func WithRelationshipCandidateTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) RelationshipCandidateOption {
	return func(p *RelationshipCandidateProvider) {
		if tp != nil {
			p.tracer = tp.Tracer("crosscodex/internal/pipeline")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			p.queryCounter, _ = meter.Int64Counter(
				"relationship.candidates.queries.total",
				metric.WithDescription("Total candidate queries"),
			)
			p.queryLatency, _ = meter.Int64Histogram(
				"relationship.candidates.query.duration_ms",
				metric.WithDescription("Candidate query duration in milliseconds"),
			)
			p.candidateGauge, _ = meter.Int64Gauge(
				"relationship.candidates.count",
				metric.WithDescription("Number of candidate pairs retrieved"),
			)
		}
	}
}

// Candidates retrieves all candidate pairs for the given tenant and job.
// It reads from the relationship_candidates table and reconstructs
// CandidatePair structs. Provenance stored alongside each row is not
// decoded here: relationship.CandidatePair has no field for it.
func (p *RelationshipCandidateProvider) Candidates(ctx context.Context, tenantID, jobID string) ([]relationship.CandidatePair, error) {
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return nil, fmt.Errorf("RelationshipCandidateProvider.Candidates: %w", err)
	}
	if jobID == "" {
		return nil, fmt.Errorf("RelationshipCandidateProvider.Candidates: job_id is required")
	}

	ctx, span := telemetry.StartSpan(p.tracer, ctx, "relationship.candidates.query")
	defer span.End()

	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.String("job_id", jobID),
	)

	start := time.Now()

	// Start tenant-scoped transaction for RLS enforcement. TenantConnection.Begin
	// already sets app.current_tenant for the session; no manual set_config here.
	tx, err := p.db.Begin(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("RelationshipCandidateProvider.Candidates: beginning transaction: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil {
			span.RecordError(rbErr)
		}
	}()

	query := `
		SELECT source_id, target_id, similarity_score
		FROM relationship_candidates
		WHERE tenant_id = $1 AND job_id = $2
		ORDER BY similarity_score DESC, source_id, target_id
	`

	rows, err := tx.Query(ctx, query, tenantID, jobID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("RelationshipCandidateProvider.Candidates: query failed: %w", err)
	}
	defer rows.Close()

	var pairs []relationship.CandidatePair
	for rows.Next() {
		var (
			sourceID   string
			targetID   string
			similarity float64
		)
		if err := rows.Scan(&sourceID, &targetID, &similarity); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("RelationshipCandidateProvider.Candidates: scanning row: %w", err)
		}

		pairs = append(pairs, relationship.CandidatePair{
			SourceControlID: sourceID,
			TargetControlID: targetID,
			SimilarityScore: float32(similarity),
		})
	}

	if err := rows.Err(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("RelationshipCandidateProvider.Candidates: rows error: %w", err)
	}

	if err := tx.Commit(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("RelationshipCandidateProvider.Candidates: commit failed: %w", err)
	}

	elapsed := time.Since(start).Milliseconds()

	// Record metrics.
	if p.queryCounter != nil {
		p.queryCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("tenant.id", tenantID),
		))
	}
	if p.queryLatency != nil {
		p.queryLatency.Record(ctx, elapsed)
	}
	if p.candidateGauge != nil {
		p.candidateGauge.Record(ctx, int64(len(pairs)), metric.WithAttributes(
			attribute.String("tenant.id", tenantID),
			attribute.String("job_id", jobID),
		))
	}

	span.SetAttributes(attribute.Int("candidate.count", len(pairs)))
	span.SetStatus(codes.Ok, "")

	return pairs, nil
}
