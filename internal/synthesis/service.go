package synthesis

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// Service orchestrates synthesis: ranking inputs, assessing quality,
// persisting viability weights, and recording OTel metrics.
type Service struct {
	db              db.TenantConnection
	cfg             config.SynthesisConfig
	actionableTypes []string
	opts            options
}

// New creates a Service with the given DB connection, config, actionable
// relationship types, and optional configuration.
func New(conn db.TenantConnection, cfg config.SynthesisConfig, actionableTypes []string, opts ...Option) *Service {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if o.logger == nil {
		o.logger = slog.Default()
	}

	return &Service{
		db:              conn,
		cfg:             cfg,
		actionableTypes: actionableTypes,
		opts:            o,
	}
}

// Execute runs the full synthesis pipeline: validate, rank, assess, persist,
// and hash.
func (s *Service) Execute(ctx context.Context, jobID string, inputs []SynthesisInput, classifications map[string]Classification) (*ExecuteResult, error) {
	start := time.Now()

	// Start span if tracer is configured.
	var span trace.Span
	if s.opts.tracer != nil {
		ctx, span = s.opts.tracer.Start(ctx, "synthesis.Execute")
		defer span.End()
	}

	// Deferred metrics recording.
	var execErr error
	defer func() {
		elapsed := float64(time.Since(start).Milliseconds())
		if s.opts.durationHist != nil {
			s.opts.durationHist.Record(ctx, elapsed)
		}
		if execErr != nil {
			if s.opts.executionCount != nil {
				s.opts.executionCount.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "error")))
			}
		} else {
			if s.opts.executionCount != nil {
				s.opts.executionCount.Add(ctx, 1, metric.WithAttributes(attribute.String("status", "ok")))
			}
		}
	}()

	// Step 1: Validate tenant.
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		execErr = err
		s.recordError(ctx, "validation")
		s.setSpanError(span, err)
		return nil, err
	}

	// Step 2: Validate jobID.
	if jobID == "" {
		execErr = fmt.Errorf("empty job ID: %w", ErrInvalidJobID)
		s.recordError(ctx, "validation")
		s.setSpanError(span, execErr)
		return nil, execErr
	}

	// Step 3: Validate inputs.
	if err := validateInputs(inputs); err != nil {
		execErr = err
		s.recordError(ctx, "validation")
		s.setSpanError(span, execErr)
		return nil, execErr
	}

	// Set span attributes after validation succeeds.
	if span != nil {
		span.SetAttributes(
			attribute.String("job.id", jobID),
			attribute.String("tenant.id", tenantID),
		)
	}

	// Capture trace ID for attestation bridge.
	traceID := telemetry.TraceIDFromContext(ctx)
	if traceID != "" && span != nil {
		span.SetAttributes(attribute.String("trace.id", traceID))
	}

	// Resolve per-tenant config.
	tenantCfg := s.cfg.ForTenant(tenantID)
	ranker := NewRanker(tenantCfg.Viability)
	assessor := NewAssessor(tenantCfg.Assessment, s.actionableTypes)
	if s.opts.tp != nil {
		ranker = ranker.WithTelemetry(s.opts.tp)
		assessor = assessor.WithTelemetry(s.opts.tp)
	}

	// Step 4: Rank.
	rows := ranker.Rank(ctx, jobID, inputs, classifications)
	s.opts.logger.InfoContext(ctx, "synthesis ranking completed", "pair_count", len(rows))
	if s.opts.pairsRanked != nil {
		s.opts.pairsRanked.Add(ctx, int64(len(rows)))
	}

	// Step 4a: Apply confidence threshold filter.
	if tenantCfg.ConfidenceThreshold > 0 {
		before := len(rows)
		rows = filterByConfidence(rows, tenantCfg.ConfidenceThreshold)
		filtered := before - len(rows)
		if filtered > 0 {
			s.opts.logger.InfoContext(ctx, "filtered rows below confidence threshold",
				"filtered", filtered,
				"threshold", tenantCfg.ConfidenceThreshold,
			)
		}
	}

	// Step 4b: Apply per-source mapping cap.
	if tenantCfg.MaxMappingsPerControl > 0 {
		rows = capMappingsPerSource(rows, tenantCfg.MaxMappingsPerControl)
	}

	// Step 5: Assess.
	report := assessor.Assess(ctx, rows)
	if report.ViablePairs == 0 {
		s.opts.logger.WarnContext(ctx, "no viable pairs after synthesis")
	}

	// Step 6: DB persistence.
	updatedCount, err := s.persistViability(ctx, span, rows, jobID, tenantID)
	if err != nil {
		execErr = err
		return nil, execErr
	}
	if s.opts.viabilityUpdates != nil {
		s.opts.viabilityUpdates.Add(ctx, int64(updatedCount))
	}

	// Step 7: Content hash.
	hash, err := s.computeContentHash(report, traceID)
	if err != nil {
		execErr = fmt.Errorf("content hash: %w", err)
		s.recordError(ctx, "hash")
		s.setSpanError(span, execErr)
		return nil, execErr
	}

	if span != nil {
		span.SetStatus(codes.Ok, "")
	}

	return &ExecuteResult{
		Rows:        rows,
		Report:      report,
		ContentHash: hash,
	}, nil
}

// filterByConfidence returns only rows whose ConfidenceFraction meets or
// exceeds the threshold.
func filterByConfidence(rows []SynthesisRow, threshold float64) []SynthesisRow {
	if rows == nil {
		return []SynthesisRow{}
	}
	out := rows[:0:0] // reuse no backing array to avoid aliasing
	for _, r := range rows {
		if r.ConfidenceFraction >= threshold {
			out = append(out, r)
		}
	}
	return out
}

// capMappingsPerSource groups rows by SourceID and keeps at most maxPerSource
// rows per source, ordered by ViabilityWeight descending.
func capMappingsPerSource(rows []SynthesisRow, maxPerSource int) []SynthesisRow {
	if maxPerSource <= 0 {
		return rows
	}

	// Group by SourceID while preserving overall insertion order for sources.
	order := make([]string, 0, len(rows))
	groups := make(map[string][]SynthesisRow, len(rows))
	for _, r := range rows {
		if _, seen := groups[r.SourceID]; !seen {
			order = append(order, r.SourceID)
		}
		groups[r.SourceID] = append(groups[r.SourceID], r)
	}

	out := make([]SynthesisRow, 0, len(rows))
	for _, src := range order {
		grp := groups[src]
		if len(grp) > maxPerSource {
			sort.Slice(grp, func(i, j int) bool {
				return grp[i].ViabilityWeight > grp[j].ViabilityWeight
			})
			grp = grp[:maxPerSource]
		}
		out = append(out, grp...)
	}
	return out
}

// setSpanError records an error on the span if it is non-nil.
func (s *Service) setSpanError(span trace.Span, err error) {
	if span == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// recordError increments the error counter with the given category.
func (s *Service) recordError(ctx context.Context, category string) {
	if s.opts.errorCount != nil {
		s.opts.errorCount.Add(ctx, 1, metric.WithAttributes(attribute.String("error_category", category)))
	}
}

// validateInputs checks all inputs for invalid fields.
//
// maxInputs and maxIDLen are intentional security ceilings, not tuning
// parameters — they exist to prevent DoS via oversized payloads. They are
// not exposed in SynthesisConfig because a user deploying CrossCodex against
// a different compliance framework would never need to raise these limits; any
// framework with >10,000 controls should be split across multiple jobs. If the
// ceilings ever need to change, they are updated here under code review, not
// via configuration. This satisfies the "documented wiring contract" requirement
// in AGENTS.md §Configuration Surface Discipline.
func validateInputs(inputs []SynthesisInput) error {
	const maxInputs = 10000
	const maxIDLen = 256

	if len(inputs) > maxInputs {
		return fmt.Errorf("too many inputs (%d, max %d): %w", len(inputs), maxInputs, ErrInvalidInput)
	}

	for i := range inputs {
		inp := &inputs[i]
		if inp.SourceID == "" {
			return fmt.Errorf("input[%d]: empty SourceID: %w", i, ErrInvalidInput)
		}
		if len(inp.SourceID) > maxIDLen {
			return fmt.Errorf("input[%d]: SourceID exceeds max length of %d: %w", i, maxIDLen, ErrInvalidInput)
		}
		if inp.TargetID == "" {
			return fmt.Errorf("input[%d]: empty TargetID: %w", i, ErrInvalidInput)
		}
		if len(inp.TargetID) > maxIDLen {
			return fmt.Errorf("input[%d]: TargetID exceeds max length of %d: %w", i, maxIDLen, ErrInvalidInput)
		}
		if math.IsNaN(inp.SimilarityScore) || math.IsInf(inp.SimilarityScore, 0) {
			return fmt.Errorf("input[%d]: invalid SimilarityScore: %w", i, ErrInvalidInput)
		}
		if inp.SimilarityScore < 0 {
			return fmt.Errorf("input[%d]: negative SimilarityScore: %w", i, ErrInvalidInput)
		}
		if inp.ConfidenceFraction < 0 || inp.ConfidenceFraction > 1 {
			return fmt.Errorf("input[%d]: ConfidenceFraction %.4f outside [0,1]: %w", i, inp.ConfidenceFraction, ErrInvalidInput)
		}
	}
	return nil
}

// persistViability writes viability weights to the database within a single
// transaction using a batch UNNEST update to avoid N+1 round-trips.
func (s *Service) persistViability(ctx context.Context, span trace.Span, rows []SynthesisRow, jobID, tenantID string) (int, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		s.recordError(ctx, "db")
		// Pass the sentinel to the span to avoid leaking pgx internal detail
		// (table names, constraint names, server error strings) into the
		// observability plane. The raw error is preserved in the returned chain
		// for structured logging by the caller.
		s.setSpanError(span, ErrDBUpdate)
		return 0, fmt.Errorf("begin transaction: %w: %w", ErrDBUpdate, err)
	}

	// Build parallel arrays for UNNEST. The DB driver passes these as typed
	// array parameters, avoiding one round-trip per row (was O(n), now O(1)).
	viabilities := make([]float64, len(rows))
	sourceIDs := make([]string, len(rows))
	targetIDs := make([]string, len(rows))
	for i := range rows {
		viabilities[i] = rows[i].ViabilityWeight
		sourceIDs[i] = rows[i].SourceID
		targetIDs[i] = rows[i].TargetID
	}

	queryRows, queryErr := tx.Query(ctx,
		`UPDATE vote_summaries AS vs
		 SET viability = u.viability
		 FROM UNNEST($1::float8[], $2::text[], $3::text[]) AS u(viability, source_id, target_id)
		 WHERE vs.job_id = $4 AND vs.source_id = u.source_id AND vs.target_id = u.target_id AND vs.tenant_id = $5
		 RETURNING vs.source_id`,
		viabilities, sourceIDs, targetIDs, jobID, tenantID,
	)
	if queryErr != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			s.opts.logger.WarnContext(ctx, "transaction rollback failed", "error", rbErr)
		}
		// Check for PostgreSQL immutability trigger (check_violation).
		if isPgErrorCode(queryErr, "23514") {
			s.opts.logger.DebugContext(ctx, "immutability violation", "error", queryErr)
			wrapped := fmt.Errorf("viability update blocked by immutability constraint — transaction rolled back, no rows updated: %w", ErrImmutabilityViolation)
			s.recordError(ctx, "db")
			s.setSpanError(span, wrapped)
			return 0, wrapped
		}
		s.opts.logger.WarnContext(ctx, "batch viability update failed", "error", queryErr)
		s.recordError(ctx, "db")
		s.setSpanError(span, ErrDBUpdate)
		return 0, fmt.Errorf("batch viability update: %w: %w", ErrDBUpdate, queryErr)
	}

	var updated int
	for queryRows.Next() {
		var sourceID string
		if scanErr := queryRows.Scan(&sourceID); scanErr != nil {
			_ = queryRows.Close()
			if rbErr := tx.Rollback(); rbErr != nil {
				s.opts.logger.WarnContext(ctx, "transaction rollback failed", "error", rbErr)
			}
			s.recordError(ctx, "db")
			s.setSpanError(span, ErrDBUpdate)
			return 0, fmt.Errorf("scan returning source_id: %w: %w", ErrDBUpdate, scanErr)
		}
		updated++
	}
	if rowsErr := queryRows.Err(); rowsErr != nil {
		_ = queryRows.Close()
		if rbErr := tx.Rollback(); rbErr != nil {
			s.opts.logger.WarnContext(ctx, "transaction rollback failed", "error", rbErr)
		}
		if isPgErrorCode(rowsErr, "23514") {
			s.opts.logger.DebugContext(ctx, "immutability violation", "error", rowsErr)
			wrapped := fmt.Errorf("viability update blocked by immutability constraint — transaction rolled back, no rows updated: %w", ErrImmutabilityViolation)
			s.recordError(ctx, "db")
			s.setSpanError(span, wrapped)
			return 0, wrapped
		}
		s.recordError(ctx, "db")
		s.setSpanError(span, ErrDBUpdate)
		return 0, fmt.Errorf("iterating viability update results: %w: %w", ErrDBUpdate, rowsErr)
	}
	_ = queryRows.Close()

	if updated < len(rows) {
		if rbErr := tx.Rollback(); rbErr != nil {
			s.opts.logger.WarnContext(ctx, "transaction rollback failed", "error", rbErr)
		}
		wrapped := fmt.Errorf("expected %d viability updates, got %d — transaction rolled back, no rows updated: %w", len(rows), updated, ErrDBNoRowsAffected)
		s.recordError(ctx, "db")
		s.setSpanError(span, wrapped)
		return 0, wrapped
	}

	if err := tx.Commit(); err != nil {
		s.recordError(ctx, "db")
		// Pass the sentinel to the span; raw commit error may contain server detail.
		s.setSpanError(span, ErrDBUpdate)
		return 0, fmt.Errorf("commit: %w: %w", ErrDBUpdate, err)
	}

	return updated, nil
}

// isPgErrorCode checks whether err (or any wrapped error) carries a
// PostgreSQL error with the given SQLSTATE code. Uses the same interface-based
// approach as pkg/db/errors.go to avoid importing pgconn directly.
func isPgErrorCode(err error, code string) bool {
	type pgErr interface {
		SQLState() string
	}
	var pe pgErr
	if errors.As(err, &pe) {
		return pe.SQLState() == code
	}
	return false
}

// goSeverityToProto converts a Go DiagnosticSeverity (0-based iota) to the
// proto DiagnosticSeverity (1-based, with UNSPECIFIED=0).
func goSeverityToProto(s DiagnosticSeverity) pb.DiagnosticSeverity {
	// Go iota: Good=0, Warn=1, Poor=2, Critical=3
	// Proto enum: UNSPECIFIED=0, GOOD=1, WARN=2, POOR=3, CRITICAL=4
	switch s {
	case SeverityGood:
		return pb.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_GOOD
	case SeverityWarn:
		return pb.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_WARN
	case SeverityPoor:
		return pb.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_POOR
	case SeverityCritical:
		return pb.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_CRITICAL
	default:
		return pb.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_UNSPECIFIED
	}
}

// computeContentHash converts a QualityReport to a proto QualityMetrics
// message and computes a deterministic SHA-256 hash via storage.ContentHash.
// traceID is the OTel trace ID from the active span; it is written into
// computed_at.correlation_id to link this record to its originating trace,
// fulfilling the AGENTS.md attestation bridge contract.
func (s *Service) computeContentHash(report *QualityReport, traceID string) (string, error) {
	typeCounts := make(map[string]int32, len(report.RelationshipCounts))
	for k, v := range report.RelationshipCounts {
		typeCounts[k] = int32(v)
	}

	pbDiagnostics := make([]*pb.DiagnosticEntry, 0, len(report.Diagnostics))
	for _, d := range report.Diagnostics {
		pbDiagnostics = append(pbDiagnostics, &pb.DiagnosticEntry{
			Category: d.Category,
			Severity: goSeverityToProto(d.Severity),
			Message:  d.Message,
			Value:    d.Value,
		})
	}

	metrics := &pb.QualityMetrics{
		TotalMappings:          int32(report.TotalPairs),
		ViableMappings:         int32(report.ViablePairs),
		AvgConfidence:          float32(report.AvgConfidence),
		AvgViability:           float32(report.AvgViability),
		RelationshipTypeCounts: typeCounts,
		Diagnostics:            pbDiagnostics,
		// Populate computed_at.correlation_id with the OTel trace ID so
		// auditors can navigate from this QualityMetrics record to the
		// originating distributed trace. See AGENTS.md attestation bridge.
		ComputedAt: &pb.AuditMetadata{
			CorrelationId: traceID,
		},
	}

	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(metrics)
	if err != nil {
		return "", fmt.Errorf("marshal quality metrics: %w", err)
	}
	return storage.ContentHash(data), nil
}
