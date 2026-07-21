package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

type Store interface {
	CreateJob(ctx context.Context, job *Job) error
	GetJob(ctx context.Context, jobID string) (*Job, error)
	ListJobs(ctx context.Context, tenantID string, filter JobFilter) ([]*Job, int64, error)
	UpdateJobStatus(ctx context.Context, jobID string, status JobStatus, jobErr error) error
	CreateStages(ctx context.Context, jobID string, stageNames []string) error
	UpdateStageStatus(ctx context.Context, jobID, stageName string, status StageStatus) error
	UpdateStageError(ctx context.Context, jobID, stageName string, stageErr error) error
	GetStages(ctx context.Context, jobID string) ([]*Stage, error)
	ResetStagesFrom(ctx context.Context, jobID, fromStage string, allStages []string) error
	GetResumableJobs(ctx context.Context) ([]*Job, error)
}

type PGStore struct {
	db       db.TenantConnection
	systemDB db.Pool
	tracer   trace.Tracer

	queryCounter metric.Int64Counter
	queryLatency metric.Float64Histogram
}

type PGStoreOption func(*PGStore)

func WithStoreTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) PGStoreOption {
	return func(s *PGStore) {
		if tp != nil {
			s.tracer = tp.Tracer("crosscodex/internal/pipeline")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			s.queryCounter, _ = meter.Int64Counter("pipeline.store.queries.total",
				metric.WithDescription("Total pipeline store queries"))
			s.queryLatency, _ = meter.Float64Histogram("pipeline.store.query.duration_ms",
				metric.WithDescription("Pipeline store query duration"))
		}
	}
}

func NewPGStore(conn db.TenantConnection, systemPool db.Pool, opts ...PGStoreOption) *PGStore {
	s := &PGStore{
		db:       conn,
		systemDB: systemPool,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

var _ Store = (*PGStore)(nil)

func (s *PGStore) CreateJob(ctx context.Context, job *Job) error {
	if err := tenant.ValidateTenantID(job.TenantID); err != nil {
		return fmt.Errorf("PGStore.CreateJob: %w", err)
	}
	if job.JobID == "" {
		return fmt.Errorf("PGStore.CreateJob: %w", ErrInvalidJobID)
	}

	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.store.create_job")
	defer span.End()

	span.SetAttributes(
		attribute.String("tenant.id", job.TenantID),
		attribute.String("job.id", job.JobID),
	)

	start := time.Now()

	tx, err := s.db.Begin(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.CreateJob: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := tx.Exec(ctx, "SET LOCAL app.current_tenant = $1", job.TenantID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.CreateJob: setting tenant: %w", err)
	}

	query := `
		INSERT INTO jobs (job_id, tenant_id, status, config, created_by, created_at, updated_at, error_message)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`

	err = tx.Exec(ctx, query, job.JobID, job.TenantID, job.Status, job.Config, job.CreatedBy, job.CreatedAt, job.UpdatedAt, job.ErrorMessage)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.CreateJob: insert failed: %w", err)
	}

	if err := tx.Commit(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.CreateJob: commit failed: %w", err)
	}

	elapsed := time.Since(start).Seconds() * 1000
	if s.queryCounter != nil {
		s.queryCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("operation", "create_job")))
	}
	if s.queryLatency != nil {
		s.queryLatency.Record(ctx, elapsed, metric.WithAttributes(attribute.String("operation", "create_job")))
	}

	span.SetStatus(codes.Ok, "")
	return nil
}

func (s *PGStore) GetJob(ctx context.Context, jobID string) (*Job, error) {
	if jobID == "" {
		return nil, fmt.Errorf("PGStore.GetJob: %w", ErrInvalidJobID)
	}

	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.store.get_job")
	defer span.End()

	span.SetAttributes(attribute.String("job.id", jobID))

	start := time.Now()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("PGStore.GetJob: %w", err)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("PGStore.GetJob: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := tx.Exec(ctx, "SET LOCAL app.current_tenant = $1", tenantID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("PGStore.GetJob: setting tenant: %w", err)
	}

	query := `
		SELECT job_id, tenant_id, status, config, created_by, created_at, updated_at, error_message
		FROM jobs
		WHERE job_id = $1 AND tenant_id = $2
	`

	row := tx.QueryRow(ctx, query, jobID, tenantID)

	var job Job
	err = row.Scan(&job.JobID, &job.TenantID, &job.Status, &job.Config, &job.CreatedBy, &job.CreatedAt, &job.UpdatedAt, &job.ErrorMessage)
	if err != nil {
		if err == sql.ErrNoRows {
			span.SetStatus(codes.Error, "not found")
			return nil, ErrNotFound
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("PGStore.GetJob: scan failed: %w", err)
	}

	if err := tx.Commit(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("PGStore.GetJob: commit failed: %w", err)
	}

	elapsed := time.Since(start).Seconds() * 1000
	if s.queryCounter != nil {
		s.queryCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("operation", "get_job")))
	}
	if s.queryLatency != nil {
		s.queryLatency.Record(ctx, elapsed, metric.WithAttributes(attribute.String("operation", "get_job")))
	}

	span.SetStatus(codes.Ok, "")
	return &job, nil
}

func (s *PGStore) ListJobs(ctx context.Context, tenantID string, filter JobFilter) ([]*Job, int64, error) {
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return nil, 0, fmt.Errorf("PGStore.ListJobs: %w", err)
	}

	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.store.list_jobs")
	defer span.End()

	span.SetAttributes(attribute.String("tenant.id", tenantID))

	start := time.Now()

	tx, err := s.db.Begin(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, 0, fmt.Errorf("PGStore.ListJobs: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := tx.Exec(ctx, "SET LOCAL app.current_tenant = $1", tenantID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, 0, fmt.Errorf("PGStore.ListJobs: setting tenant: %w", err)
	}

	// Build query with optional status filter
	countQuery := "SELECT COUNT(*) FROM jobs WHERE tenant_id = $1"
	listQuery := "SELECT job_id, tenant_id, status, config, created_by, created_at, updated_at, error_message FROM jobs WHERE tenant_id = $1"
	args := []interface{}{tenantID}

	if filter.Status != "" {
		countQuery += " AND status = $2"
		listQuery += " AND status = $2"
		args = append(args, filter.Status)
	}

	// Get total count
	row := tx.QueryRow(ctx, countQuery, args...)
	var total int64
	if err := row.Scan(&total); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, 0, fmt.Errorf("PGStore.ListJobs: count failed: %w", err)
	}

	// Apply pagination
	listQuery += " ORDER BY created_at DESC"
	if filter.Limit > 0 {
		listQuery += fmt.Sprintf(" LIMIT %d", filter.Limit)
	}
	if filter.Offset > 0 {
		listQuery += fmt.Sprintf(" OFFSET %d", filter.Offset)
	}

	rows, err := tx.Query(ctx, listQuery, args...)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, 0, fmt.Errorf("PGStore.ListJobs: query failed: %w", err)
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.JobID, &job.TenantID, &job.Status, &job.Config, &job.CreatedBy, &job.CreatedAt, &job.UpdatedAt, &job.ErrorMessage); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, 0, fmt.Errorf("PGStore.ListJobs: scan failed: %w", err)
		}
		jobs = append(jobs, &job)
	}

	if err := rows.Err(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, 0, fmt.Errorf("PGStore.ListJobs: rows error: %w", err)
	}

	if err := tx.Commit(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, 0, fmt.Errorf("PGStore.ListJobs: commit failed: %w", err)
	}

	elapsed := time.Since(start).Seconds() * 1000
	if s.queryCounter != nil {
		s.queryCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("operation", "list_jobs")))
	}
	if s.queryLatency != nil {
		s.queryLatency.Record(ctx, elapsed, metric.WithAttributes(attribute.String("operation", "list_jobs")))
	}

	span.SetStatus(codes.Ok, "")
	return jobs, total, nil
}

func (s *PGStore) UpdateJobStatus(ctx context.Context, jobID string, status JobStatus, jobErr error) error {
	if jobID == "" {
		return fmt.Errorf("PGStore.UpdateJobStatus: %w", ErrInvalidJobID)
	}

	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.store.update_job_status")
	defer span.End()

	span.SetAttributes(
		attribute.String("job.id", jobID),
		attribute.String("status", string(status)),
	)

	start := time.Now()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.UpdateJobStatus: %w", err)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.UpdateJobStatus: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := tx.Exec(ctx, "SET LOCAL app.current_tenant = $1", tenantID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.UpdateJobStatus: setting tenant: %w", err)
	}

	errorMessage := ""
	if jobErr != nil {
		errorMessage = jobErr.Error()
	}

	query := `
		UPDATE jobs
		SET status = $1, updated_at = $2, error_message = $3
		WHERE job_id = $4 AND tenant_id = $5
	`

	err = tx.Exec(ctx, query, status, time.Now(), errorMessage, jobID, tenantID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.UpdateJobStatus: update failed: %w", err)
	}

	if err := tx.Commit(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.UpdateJobStatus: commit failed: %w", err)
	}

	elapsed := time.Since(start).Seconds() * 1000
	if s.queryCounter != nil {
		s.queryCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("operation", "update_job_status")))
	}
	if s.queryLatency != nil {
		s.queryLatency.Record(ctx, elapsed, metric.WithAttributes(attribute.String("operation", "update_job_status")))
	}

	span.SetStatus(codes.Ok, "")
	return nil
}

func (s *PGStore) CreateStages(ctx context.Context, jobID string, stageNames []string) error {
	if jobID == "" {
		return fmt.Errorf("PGStore.CreateStages: %w", ErrInvalidJobID)
	}

	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.store.create_stages")
	defer span.End()

	span.SetAttributes(
		attribute.String("job.id", jobID),
		attribute.Int("stage.count", len(stageNames)),
	)

	start := time.Now()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.CreateStages: %w", err)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.CreateStages: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := tx.Exec(ctx, "SET LOCAL app.current_tenant = $1", tenantID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.CreateStages: setting tenant: %w", err)
	}

	query := `
		INSERT INTO job_stages (job_id, stage_name, status, tenant_id)
		VALUES ($1, $2, $3, $4)
	`

	for _, stageName := range stageNames {
		if err := tx.Exec(ctx, query, jobID, stageName, StageStatusPending, tenantID); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return fmt.Errorf("PGStore.CreateStages: insert stage %s failed: %w", stageName, err)
		}
	}

	if err := tx.Commit(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.CreateStages: commit failed: %w", err)
	}

	elapsed := time.Since(start).Seconds() * 1000
	if s.queryCounter != nil {
		s.queryCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("operation", "create_stages")))
	}
	if s.queryLatency != nil {
		s.queryLatency.Record(ctx, elapsed, metric.WithAttributes(attribute.String("operation", "create_stages")))
	}

	span.SetStatus(codes.Ok, "")
	return nil
}

func (s *PGStore) UpdateStageStatus(ctx context.Context, jobID, stageName string, status StageStatus) error {
	if jobID == "" {
		return fmt.Errorf("PGStore.UpdateStageStatus: %w", ErrInvalidJobID)
	}

	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.store.update_stage_status")
	defer span.End()

	span.SetAttributes(
		attribute.String("job.id", jobID),
		attribute.String("stage.name", stageName),
		attribute.String("status", string(status)),
	)

	start := time.Now()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.UpdateStageStatus: %w", err)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.UpdateStageStatus: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := tx.Exec(ctx, "SET LOCAL app.current_tenant = $1", tenantID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.UpdateStageStatus: setting tenant: %w", err)
	}

	// Set started_at when status is running; set completed_at when completed or failed
	var query string
	switch status {
	case StageStatusRunning:
		query = `
			UPDATE job_stages
			SET status = $1, started_at = $2
			WHERE job_id = $3 AND stage_name = $4 AND tenant_id = $5
		`
		err = tx.Exec(ctx, query, status, time.Now(), jobID, stageName, tenantID)
	case StageStatusCompleted, StageStatusFailed:
		query = `
			UPDATE job_stages
			SET status = $1, completed_at = $2
			WHERE job_id = $3 AND stage_name = $4 AND tenant_id = $5
		`
		err = tx.Exec(ctx, query, status, time.Now(), jobID, stageName, tenantID)
	default:
		query = `
			UPDATE job_stages
			SET status = $1
			WHERE job_id = $2 AND stage_name = $3 AND tenant_id = $4
		`
		err = tx.Exec(ctx, query, status, jobID, stageName, tenantID)
	}

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.UpdateStageStatus: update failed: %w", err)
	}

	if err := tx.Commit(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.UpdateStageStatus: commit failed: %w", err)
	}

	elapsed := time.Since(start).Seconds() * 1000
	if s.queryCounter != nil {
		s.queryCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("operation", "update_stage_status")))
	}
	if s.queryLatency != nil {
		s.queryLatency.Record(ctx, elapsed, metric.WithAttributes(attribute.String("operation", "update_stage_status")))
	}

	span.SetStatus(codes.Ok, "")
	return nil
}

func (s *PGStore) UpdateStageError(ctx context.Context, jobID, stageName string, stageErr error) error {
	if jobID == "" {
		return fmt.Errorf("PGStore.UpdateStageError: %w", ErrInvalidJobID)
	}

	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.store.update_stage_error")
	defer span.End()

	span.SetAttributes(
		attribute.String("job.id", jobID),
		attribute.String("stage.name", stageName),
	)

	start := time.Now()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.UpdateStageError: %w", err)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.UpdateStageError: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := tx.Exec(ctx, "SET LOCAL app.current_tenant = $1", tenantID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.UpdateStageError: setting tenant: %w", err)
	}

	errorMessage := ""
	if stageErr != nil {
		errorMessage = stageErr.Error()
	}

	query := `
		UPDATE job_stages
		SET status = $1, error_message = $2, completed_at = $3
		WHERE job_id = $4 AND stage_name = $5 AND tenant_id = $6
	`

	err = tx.Exec(ctx, query, StageStatusFailed, errorMessage, time.Now(), jobID, stageName, tenantID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.UpdateStageError: update failed: %w", err)
	}

	if err := tx.Commit(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.UpdateStageError: commit failed: %w", err)
	}

	elapsed := time.Since(start).Seconds() * 1000
	if s.queryCounter != nil {
		s.queryCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("operation", "update_stage_error")))
	}
	if s.queryLatency != nil {
		s.queryLatency.Record(ctx, elapsed, metric.WithAttributes(attribute.String("operation", "update_stage_error")))
	}

	span.SetStatus(codes.Ok, "")
	return nil
}

func (s *PGStore) GetStages(ctx context.Context, jobID string) ([]*Stage, error) {
	if jobID == "" {
		return nil, fmt.Errorf("PGStore.GetStages: %w", ErrInvalidJobID)
	}

	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.store.get_stages")
	defer span.End()

	span.SetAttributes(attribute.String("job.id", jobID))

	start := time.Now()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("PGStore.GetStages: %w", err)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("PGStore.GetStages: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := tx.Exec(ctx, "SET LOCAL app.current_tenant = $1", tenantID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("PGStore.GetStages: setting tenant: %w", err)
	}

	query := `
		SELECT job_id, stage_name, status, started_at, completed_at, retry_count, error_message, tenant_id
		FROM job_stages
		WHERE job_id = $1 AND tenant_id = $2
		ORDER BY stage_name
	`

	rows, err := tx.Query(ctx, query, jobID, tenantID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("PGStore.GetStages: query failed: %w", err)
	}
	defer rows.Close()

	var stages []*Stage
	for rows.Next() {
		var stage Stage
		if err := rows.Scan(&stage.JobID, &stage.StageName, &stage.Status, &stage.StartedAt, &stage.CompletedAt, &stage.RetryCount, &stage.ErrorMessage, &stage.TenantID); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("PGStore.GetStages: scan failed: %w", err)
		}
		stages = append(stages, &stage)
	}

	if err := rows.Err(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("PGStore.GetStages: rows error: %w", err)
	}

	if err := tx.Commit(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("PGStore.GetStages: commit failed: %w", err)
	}

	elapsed := time.Since(start).Seconds() * 1000
	if s.queryCounter != nil {
		s.queryCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("operation", "get_stages")))
	}
	if s.queryLatency != nil {
		s.queryLatency.Record(ctx, elapsed, metric.WithAttributes(attribute.String("operation", "get_stages")))
	}

	span.SetStatus(codes.Ok, "")
	return stages, nil
}

func (s *PGStore) ResetStagesFrom(ctx context.Context, jobID, fromStage string, allStages []string) error {
	if jobID == "" {
		return fmt.Errorf("PGStore.ResetStagesFrom: %w", ErrInvalidJobID)
	}

	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.store.reset_stages_from")
	defer span.End()

	span.SetAttributes(
		attribute.String("job.id", jobID),
		attribute.String("from.stage", fromStage),
	)

	start := time.Now()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.ResetStagesFrom: %w", err)
	}

	// Find index of fromStage in allStages
	fromIndex := -1
	for i, stage := range allStages {
		if stage == fromStage {
			fromIndex = i
			break
		}
	}

	if fromIndex == -1 {
		err := fmt.Errorf("stage %s not found in allStages", fromStage)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.ResetStagesFrom: %w", err)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.ResetStagesFrom: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := tx.Exec(ctx, "SET LOCAL app.current_tenant = $1", tenantID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.ResetStagesFrom: setting tenant: %w", err)
	}

	query := `
		UPDATE job_stages
		SET status = $1, started_at = NULL, completed_at = NULL, error_message = '', retry_count = retry_count + 1
		WHERE job_id = $2 AND stage_name = $3 AND tenant_id = $4
	`

	// Reset fromStage and all stages after it
	for i := fromIndex; i < len(allStages); i++ {
		if err := tx.Exec(ctx, query, StageStatusPending, jobID, allStages[i], tenantID); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return fmt.Errorf("PGStore.ResetStagesFrom: reset stage %s failed: %w", allStages[i], err)
		}
	}

	if err := tx.Commit(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("PGStore.ResetStagesFrom: commit failed: %w", err)
	}

	elapsed := time.Since(start).Seconds() * 1000
	if s.queryCounter != nil {
		s.queryCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("operation", "reset_stages_from")))
	}
	if s.queryLatency != nil {
		s.queryLatency.Record(ctx, elapsed, metric.WithAttributes(attribute.String("operation", "reset_stages_from")))
	}

	span.SetStatus(codes.Ok, "")
	return nil
}

func (s *PGStore) GetResumableJobs(ctx context.Context) ([]*Job, error) {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.store.get_resumable_jobs")
	defer span.End()

	start := time.Now()

	// System-level query - NO tenant scoping, no RLS.
	// Use systemDB Pool directly instead of TenantConnection.
	query := `
		SELECT job_id, tenant_id, status, config, created_by, created_at, updated_at, error_message
		FROM jobs
		WHERE status = $1
		ORDER BY created_at
	`

	rows, err := s.systemDB.Query(ctx, query, JobStatusRunning)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("PGStore.GetResumableJobs: query failed: %w", err)
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		var job Job
		if err := rows.Scan(&job.JobID, &job.TenantID, &job.Status, &job.Config, &job.CreatedBy, &job.CreatedAt, &job.UpdatedAt, &job.ErrorMessage); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("PGStore.GetResumableJobs: scan failed: %w", err)
		}
		jobs = append(jobs, &job)
	}

	if err := rows.Err(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("PGStore.GetResumableJobs: rows error: %w", err)
	}

	elapsed := time.Since(start).Seconds() * 1000
	if s.queryCounter != nil {
		s.queryCounter.Add(ctx, 1, metric.WithAttributes(attribute.String("operation", "get_resumable_jobs")))
	}
	if s.queryLatency != nil {
		s.queryLatency.Record(ctx, elapsed, metric.WithAttributes(attribute.String("operation", "get_resumable_jobs")))
	}

	span.SetStatus(codes.Ok, "")
	return jobs, nil
}
