package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/internal/analyzer/requires"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// RequiresCandidateProvider implements the requires.CandidateProvider interface
// by reading candidate pairs from the requires_candidates table. This bridges
// candidate generation (which writes to the table) to the requires analyzer
// (which needs pairs to vote on).
type RequiresCandidateProvider struct {
	db     db.TenantConnection
	tracer trace.Tracer

	// Metrics (optional, nil-safe)
	queryCounter   metric.Int64Counter
	queryLatency   metric.Int64Histogram
	candidateGauge metric.Int64Gauge
}

// Compile-time check that RequiresCandidateProvider implements requires.CandidateProvider.
var _ requires.CandidateProvider = (*RequiresCandidateProvider)(nil)

// NewRequiresCandidateProvider creates a provider with the given database connection.
func NewRequiresCandidateProvider(conn db.TenantConnection, opts ...RequiresCandidateOption) *RequiresCandidateProvider {
	p := &RequiresCandidateProvider{
		db: conn,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// RequiresCandidateOption configures the RequiresCandidateProvider.
type RequiresCandidateOption func(*RequiresCandidateProvider)

// WithCandidateTelemetry enables tracing and metrics for the provider.
func WithCandidateTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) RequiresCandidateOption {
	return func(p *RequiresCandidateProvider) {
		if tp != nil {
			p.tracer = tp.Tracer("crosscodex/internal/pipeline")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			p.queryCounter, _ = meter.Int64Counter(
				"requires.candidates.queries.total",
				metric.WithDescription("Total candidate queries"),
			)
			p.queryLatency, _ = meter.Int64Histogram(
				"requires.candidates.query.duration_ms",
				metric.WithDescription("Candidate query duration in milliseconds"),
			)
			p.candidateGauge, _ = meter.Int64Gauge(
				"requires.candidates.count",
				metric.WithDescription("Number of candidate pairs retrieved"),
			)
		}
	}
}

// Candidates retrieves all candidate pairs for the given tenant and job.
// It reads from the requires_candidates table and reconstructs RequiresPair
// structs with full provenance information.
func (p *RequiresCandidateProvider) Candidates(ctx context.Context, tenantID, jobID string) ([]requires.RequiresPair, error) {
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return nil, fmt.Errorf("RequiresCandidateProvider.Candidates: %w", err)
	}
	if jobID == "" {
		return nil, fmt.Errorf("RequiresCandidateProvider.Candidates: job_id is required")
	}

	ctx, span := telemetry.StartSpan(p.tracer, ctx, "requires.candidates.query")
	defer span.End()

	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.String("job_id", jobID),
	)

	start := time.Now()

	// Start tenant-scoped transaction for RLS enforcement.
	tx, err := p.db.Begin(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("RequiresCandidateProvider.Candidates: beginning transaction: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil {
			span.RecordError(rbErr)
		}
	}()

	// Set tenant context for RLS.
	if err := tx.Exec(ctx, "SET LOCAL app.current_tenant = $1", tenantID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("RequiresCandidateProvider.Candidates: setting tenant: %w", err)
	}

	query := `
		SELECT source_id, target_id, aggregate_score, provenance
		FROM requires_candidates
		WHERE tenant_id = $1 AND job_id = $2
		ORDER BY aggregate_score DESC, source_id, target_id
	`

	rows, err := tx.Query(ctx, query, tenantID, jobID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("RequiresCandidateProvider.Candidates: query failed: %w", err)
	}
	defer rows.Close()

	var pairs []requires.RequiresPair
	for rows.Next() {
		var (
			sourceID       string
			targetID       string
			aggregateScore float64
			provenanceJSON []byte
		)
		if err := rows.Scan(&sourceID, &targetID, &aggregateScore, &provenanceJSON); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("RequiresCandidateProvider.Candidates: scanning row: %w", err)
		}

		// Unmarshal provenance from JSONB.
		var provenance []requires.CandidateProvenance
		if err := json.Unmarshal(provenanceJSON, &provenance); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("RequiresCandidateProvider.Candidates: unmarshaling provenance for %s--%s: %w",
				sourceID, targetID, err)
		}

		pairs = append(pairs, requires.RequiresPair{
			SourceControlID: sourceID,
			TargetControlID: targetID,
			AggregateScore:  aggregateScore,
			Provenance:      provenance,
		})
	}

	if err := rows.Err(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("RequiresCandidateProvider.Candidates: rows error: %w", err)
	}

	if err := tx.Commit(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("RequiresCandidateProvider.Candidates: commit failed: %w", err)
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
