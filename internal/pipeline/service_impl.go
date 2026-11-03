package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/jobconfig"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func (s *Service) checkConcurrencyLimit() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	runningCount := len(s.running)
	if s.cfg.MaxConcurrentJobs > 0 && runningCount >= s.cfg.MaxConcurrentJobs {
		return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("max concurrent jobs reached (%d)", s.cfg.MaxConcurrentJobs))
	}
	return nil
}

// validateJobConfig refuses a job config no job may be created from. It
// guards every path that writes a new job row (CreateJob, and RetryJob's new
// job), so a stored config always names its catalogs consistently. The
// message is the caller's to act on.
func validateJobConfig(cfg *pb.JobConfig) error {
	source, target := cfg.GetCatalogId(), cfg.GetTargetCatalogId()
	if source == "" {
		return errors.New("config.catalog_id is required: an analysis job runs over an imported catalog. " +
			"Import the catalog first, then start the analysis with its catalog_id")
	}
	if target == "" {
		return fmt.Errorf("config.target_catalog_id is required: an analysis job maps the controls of config.catalog_id onto a target catalog. "+
			"Set it to the catalog to map onto, or set it to config.catalog_id to analyse within one catalog "+
			"(crosscodex run start --source %s --target %s)", source, source)
	}
	if cfg.GetBidirectional() && target == source {
		return errors.New("config.bidirectional cannot be set when config.target_catalog_id equals config.catalog_id: " +
			"an analysis within one catalog already pairs its controls in both directions. " +
			"Unset bidirectional, or set target_catalog_id to a different catalog")
	}
	return nil
}

// snapshotTenantCatalogs refuses a job whose source or target is not a
// catalog of tenantID, before any job row exists, and otherwise sets cfg's
// source_catalog and target_catalog to each catalog's current name and
// version, overwriting whatever the caller sent: the job keeps the labels it
// was created under after a re-import renames a catalog. The gateway checks
// the catalogs too, but CreateJob is the authoritative entry point and the
// gateway is not its only caller. A missing catalog and another tenant's
// catalog get one fixed message, built here rather than from the reader's
// error, so the answer is no existence oracle. Without a reader nothing can
// be confirmed, so every job is refused (fail closed). operation names what
// is refused in every error ("create the job", "retry job X as a new job").
func (s *Service) snapshotTenantCatalogs(ctx context.Context, tenantID, operation string, cfg *pb.JobConfig) error {
	if s.catalogNames == nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf(
			"cannot %s: no catalog reader is configured, so the pipeline cannot confirm that the job's catalogs "+
				"are your tenant's. An operator must wire the catalog name reader (pipeline.WithCatalogNameReader)", operation))
	}
	type check struct {
		field, id string
		snapshot  **pb.CatalogSnapshot // the cfg field the catalog's label goes to
	}
	checks := []check{{"catalog_id", cfg.GetCatalogId(), &cfg.SourceCatalog}}
	if cfg.GetTargetCatalogId() != cfg.GetCatalogId() {
		checks = append(checks, check{"target_catalog_id", cfg.GetTargetCatalogId(), &cfg.TargetCatalog})
	}
	for _, c := range checks {
		snap, err := s.catalogNames.CatalogSnapshot(ctx, tenantID, c.id)
		switch {
		case errors.Is(err, ErrCatalogNotFound):
			return connect.NewError(connect.CodeNotFound, fmt.Errorf(
				"cannot %s: config.%s %q is not a catalog of your tenant: no imported catalog has that ID. "+
					"Import the catalog first, or run \"crosscodex catalog list\" to find the ID of an imported catalog", operation, c.field, c.id))
		case err != nil:
			// The cause (a database error) is withheld from the caller
			// (CWE-209) and logged once here.
			s.logger.ErrorContext(ctx, "catalog read failed", "operation", operation,
				"tenant", tenantID, "field", c.field, "catalog_id", c.id, "error", err)
			return connect.NewError(connect.CodeUnavailable, fmt.Errorf(
				"cannot %s: whether config.%s %q is a catalog of your tenant could not be checked, "+
					"because the catalog store could not be read. Retry the request; if it keeps failing, "+
					"ask an operator to check the database", operation, c.field, c.id))
		}
		*c.snapshot = snap
	}
	if cfg.GetTargetCatalogId() == cfg.GetCatalogId() {
		cfg.TargetCatalog = proto.Clone(cfg.GetSourceCatalog()).(*pb.CatalogSnapshot)
	}
	return nil
}

func (s *Service) CreateJob(ctx context.Context, req *connect.Request[pb.CreateJobRequest]) (*connect.Response[pb.CreateJobResponse], error) {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.CreateJob")
	defer span.End()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing tenant context"))
	}

	span.SetAttributes(attribute.String("tenant.id", tenantID))

	if req.Msg.Config == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("config is required"))
	}
	if err := validateJobConfig(req.Msg.Config); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	// A copy, so recording the snapshot leaves the caller's message as sent.
	cfg := proto.Clone(req.Msg.Config).(*pb.JobConfig)
	if err := s.snapshotTenantCatalogs(ctx, tenantID, "create the job", cfg); err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, err
	}

	dag, err := s.registry.BuildDAG(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("build DAG: %v", err))
	}

	jobID := uuid.New().String()

	configBytes, err := json.Marshal(cfg)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("marshal config: %v", err))
	}

	// Check concurrency limit BEFORE creating job in DB.
	if err := s.checkConcurrencyLimit(); err != nil {
		return nil, err
	}

	now := time.Now()
	job := &Job{
		JobID:     jobID,
		TenantID:  tenantID,
		Status:    JobStatusPending,
		Config:    configBytes,
		CreatedBy: JobCreator(ctx, tenantID),
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := s.store.CreateJob(ctx, job); err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create job: %v", err))
	}

	stageNames := buildStageNames(dag.Order())

	if err := s.store.CreateStages(ctx, jobID, stageNames); err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create stages: %v", err))
	}

	// Register cancel func after successful DB creation.
	s.mu.Lock()
	jobCtx, cancel := context.WithCancelCause(context.Background())
	rj := &runningJob{cancel: cancel}
	s.running[jobID] = rj
	s.mu.Unlock()

	s.wg.Add(1)
	go s.executeJob(jobCtx, rj, tenantID, jobID)

	if err := s.publishJobState(ctx, tenantID, jobID, JobStatusPending); err != nil {
		s.logger.WarnContext(ctx, "failed to publish job state", "job_id", jobID, "error", err)
	}

	span.SetStatus(otelcodes.Ok, "")
	return connect.NewResponse(&pb.CreateJobResponse{
		JobId:  jobID,
		Status: jobStatusToProto(JobStatusPending),
	}), nil
}

func (s *Service) GetJob(ctx context.Context, req *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error) {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.GetJob")
	defer span.End()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing tenant context"))
	}

	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.String("job.id", req.Msg.JobId),
	)

	job, err := s.store.GetJob(ctx, req.Msg.JobId)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("job not found"))
		}
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get job: %v", err))
	}

	stages, err := s.store.GetStages(ctx, req.Msg.JobId)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get stages: %v", err))
	}

	pbJob := jobToProto(job, stages)

	span.SetStatus(otelcodes.Ok, "")
	return connect.NewResponse(&pb.GetJobResponse{Job: pbJob}), nil
}

func (s *Service) ListJobs(ctx context.Context, req *connect.Request[pb.ListJobsRequest]) (*connect.Response[pb.ListJobsResponse], error) {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.ListJobs")
	defer span.End()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing tenant context"))
	}

	span.SetAttributes(attribute.String("tenant.id", tenantID))

	var pageSize int32 = 50
	var pageToken string
	if req.Msg.Options != nil && req.Msg.Options.Pagination != nil {
		if req.Msg.Options.Pagination.PageSize > 0 {
			pageSize = req.Msg.Options.Pagination.PageSize
		}
		pageToken = req.Msg.Options.Pagination.PageToken
	}

	offset := 0
	if pageToken != "" {
		if _, err := fmt.Sscanf(pageToken, "%d", &offset); err != nil {
			s.logger.WarnContext(ctx, "invalid page token, resetting to first page",
				"page_token", pageToken, "error", err)
		}
	}

	filter := JobFilter{
		Limit:  int(pageSize),
		Offset: offset,
	}
	if req.Msg.Status != pb.JobStatus_JOB_STATUS_UNSPECIFIED {
		filter.Status = jobStatusFromProto(req.Msg.Status)
	}

	jobs, total, err := s.store.ListJobs(ctx, tenantID, filter)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list jobs: %v", err))
	}

	pbJobs := make([]*pb.PipelineJob, len(jobs))
	for i, job := range jobs {
		stages, err := s.store.GetStages(ctx, job.JobID)
		if err != nil {
			s.logger.WarnContext(ctx, "failed to get stages for job", "job_id", job.JobID, "error", err)
			stages = nil
		}
		pbJobs[i] = jobToProto(job, stages)
	}

	nextPageToken := ""
	if int64(filter.Offset+filter.Limit) < total {
		nextPageToken = fmt.Sprintf("%d", filter.Offset+filter.Limit)
	}

	span.SetStatus(otelcodes.Ok, "")
	return connect.NewResponse(&pb.ListJobsResponse{
		Jobs: pbJobs,
		PageInfo: &pb.PageInfo{
			NextPageToken: nextPageToken,
			TotalCount:    total,
		},
	}), nil
}

func (s *Service) CancelJob(ctx context.Context, req *connect.Request[pb.CancelJobRequest]) (*connect.Response[pb.CancelJobResponse], error) {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.CancelJob")
	defer span.End()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing tenant context"))
	}

	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.String("job.id", req.Msg.JobId),
	)

	if _, err := s.store.GetJob(ctx, req.Msg.JobId); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("job not found"))
		}
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get job: %v", err))
	}

	s.mu.Lock()
	rj, ok := s.running[req.Msg.JobId]
	if !ok {
		s.mu.Unlock()
		return nil, connect.NewError(connect.CodeNotFound, errors.New("job is not running"))
	}
	if rj.completing {
		s.mu.Unlock()
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"cannot cancel job %s: it is already completing (every stage has run and its attestation is stored). "+
				"Check its final status with \"crosscodex run status %s\"", req.Msg.JobId, req.Msg.JobId))
	}
	if rj.cancelled {
		s.mu.Unlock()
		return nil, connect.NewError(connect.CodeNotFound, errors.New("job is not running"))
	}
	rj.cancel(ErrJobCancelled)
	rj.cancelled = true
	s.mu.Unlock()

	// executeJob sees ErrJobCancelled as its context's cause, records the
	// cancellation (interruptJob), and removes the entry when it returns.

	span.SetStatus(otelcodes.Ok, "")
	return connect.NewResponse(&pb.CancelJobResponse{
		Cancelled: true,
	}), nil
}

func (s *Service) RetryJob(ctx context.Context, req *connect.Request[pb.RetryJobRequest]) (*connect.Response[pb.RetryJobResponse], error) {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.RetryJob")
	defer span.End()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing tenant context"))
	}

	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.String("job.id", req.Msg.JobId),
	)

	job, err := s.store.GetJob(ctx, req.Msg.JobId)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("job not found"))
		}
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get job: %v", err))
	}

	release, err := s.reserveRetry(job)
	if err != nil {
		return nil, err
	}
	defer release()

	// The stored error message is the job's record of its failure and is
	// quoted unchanged. A command in it may predate the current CLI (a job
	// that failed before target_catalog_id names no --target), so the
	// refusal ends with a command built from the job's config instead.
	if refusal := retryRefusal(job.ErrorClass); refusal != "" && req.Msg.RetryFromFailure {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"cannot retry job %s from its failure: %s. It failed with (as recorded then; a command in it may predate this version): %s. %s",
			job.JobID, refusal, job.ErrorMessage, retryRemedy(job)))
	}
	if job.ErrorClass == JobErrorEmptyTargetCatalog && !req.Msg.RetryFromFailure {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"cannot retry job %s as a new job: its target catalog has no control with a statement, "+
				"and a new job with the same config reads the same catalog. %s",
			job.JobID, retryRemedy(job)))
	}
	// A job started with --no-cache looks nothing up, so a new job with its
	// config is the --no-cache remedy itself. An unreadable config reads as
	// a job that did look up.
	cats, _ := jobconfig.ExtractCatalogs(job.Config)
	if job.ErrorClass == JobErrorResultCacheCorrupt && !req.Msg.RetryFromFailure && !cats.NoCache {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"cannot retry job %s as a new job: a cached result it would reuse failed its integrity check, "+
				"or a stage run's cache record did not match what the stage wrote, "+
				"and a new job with the same config reads the same cache. %s",
			job.JobID, retryRemedy(job)))
	}

	// Check concurrency limit before spawning retry job.
	if err := s.checkConcurrencyLimit(); err != nil {
		return nil, err
	}

	var newJobID string

	if req.Msg.RetryFromFailure {
		stages, err := s.store.GetStages(ctx, req.Msg.JobId)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(otelcodes.Error, err.Error())
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get stages: %v", err))
		}

		// A cancelled stage is retried like a failed one: the cancel
		// stopped the job, and the stages before it completed soundly.
		var firstFailed string
		stageNames := make([]string, len(stages))
		for i, stage := range stages {
			stageNames[i] = stage.StageName
			if firstFailed == "" && (stage.Status == StageStatusFailed || stage.Status == StageStatusCancelled) {
				firstFailed = stage.StageName
			}
		}

		// A failed or cancelled job with no failed or cancelled stage
		// stopped between stages, such as while its attestation layout, an
		// analyzer's link or its bundle was being signed or stored.
		// Resuming it runs only what did not complete, so no stage is
		// reset.
		resumeOnly := firstFailed == "" && (job.Status == JobStatusFailed || job.Status == JobStatusCancelled)

		if firstFailed == "" && !resumeOnly {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
				"cannot retry job %s from a failure: no stage failed or was cancelled, so there is no stage to resume from. "+
					"To analyse the catalogs again, start a new job: %s", req.Msg.JobId, newJobCommand(job)))
		}

		if !resumeOnly {
			if err := s.store.ResetStagesFrom(ctx, req.Msg.JobId, firstFailed, stageNames); err != nil {
				span.RecordError(err)
				span.SetStatus(otelcodes.Error, err.Error())
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("reset stages: %v", err))
			}
		}

		if err := s.store.UpdateJobStatus(ctx, req.Msg.JobId, JobStatusRunning, nil); err != nil {
			span.RecordError(err)
			span.SetStatus(otelcodes.Error, err.Error())
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update job status: %v", err))
		}

		s.mu.Lock()
		jobCtx, cancel := context.WithCancelCause(context.Background())
		rj := &runningJob{cancel: cancel}
		s.running[req.Msg.JobId] = rj
		s.mu.Unlock()

		s.wg.Add(1)
		go s.executeJob(jobCtx, rj, tenantID, req.Msg.JobId)

		newJobID = req.Msg.JobId
	} else {
		cfg, err := retryJobConfig(job)
		if err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		// The new job is created now, so it records the catalogs' labels
		// now, and is confirmed to be over the tenant's catalogs as
		// CreateJob confirms a job.
		if err := s.snapshotTenantCatalogs(ctx, tenantID, fmt.Sprintf("retry job %s as a new job", job.JobID), cfg); err != nil {
			span.RecordError(err)
			span.SetStatus(otelcodes.Error, err.Error())
			return nil, err
		}
		configBytes, err := json.Marshal(cfg)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("cannot retry job %s as a new job: its config could not be encoded: %w", job.JobID, err))
		}

		newJobID = uuid.New().String()

		now := time.Now()
		newJob := &Job{
			JobID:     newJobID,
			TenantID:  tenantID,
			Status:    JobStatusPending,
			Config:    configBytes,
			CreatedBy: JobCreator(ctx, tenantID),
			CreatedAt: now,
			UpdatedAt: now,
		}

		if err := s.store.CreateJob(ctx, newJob); err != nil {
			span.RecordError(err)
			span.SetStatus(otelcodes.Error, err.Error())
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create job: %v", err))
		}

		dag, err := s.registry.BuildDAG(ctx)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(otelcodes.Error, err.Error())
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("build DAG: %v", err))
		}

		stageNames := buildStageNames(dag.Order())

		if err := s.store.CreateStages(ctx, newJobID, stageNames); err != nil {
			span.RecordError(err)
			span.SetStatus(otelcodes.Error, err.Error())
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create stages: %v", err))
		}

		s.mu.Lock()
		jobCtx, cancel := context.WithCancelCause(context.Background())
		rj := &runningJob{cancel: cancel}
		s.running[newJobID] = rj
		s.mu.Unlock()

		s.wg.Add(1)
		go s.executeJob(jobCtx, rj, tenantID, newJobID)
	}

	if err := s.publishJobState(ctx, tenantID, newJobID, JobStatusRunning); err != nil {
		s.logger.WarnContext(ctx, "failed to publish job state", "job_id", newJobID, "error", err)
	}

	span.SetStatus(otelcodes.Ok, "")
	return connect.NewResponse(&pb.RetryJobResponse{
		NewJobId: newJobID,
	}), nil
}

func (s *Service) GetJobTrace(ctx context.Context, req *connect.Request[pb.GetJobTraceRequest]) (*connect.Response[pb.GetJobTraceResponse], error) {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.GetJobTrace")
	defer span.End()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing tenant context"))
	}

	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.String("job.id", req.Msg.JobId),
	)

	span.SetStatus(otelcodes.Ok, "")
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("GetJobTrace is not yet implemented"))
}

func (s *Service) Start(ctx context.Context) error {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.Start")
	defer span.End()

	jobs, err := s.store.GetResumableJobs(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return fmt.Errorf("pipeline: get resumable jobs: %w", err)
	}

	s.logger.InfoContext(ctx, "resuming interrupted jobs", "count", len(jobs))

	// Bound concurrent resumed executions to MaxConcurrentJobs so a crash that
	// left many jobs 'running' does not launch them all at once on restart. A
	// non-positive limit means unbounded, mirroring checkConcurrencyLimit.
	var sem chan struct{}
	if s.cfg.MaxConcurrentJobs > 0 {
		sem = make(chan struct{}, s.cfg.MaxConcurrentJobs)
	}

	s.wg.Add(1)
	go s.dispatchResumableJobs(jobs, sem)

	span.SetStatus(otelcodes.Ok, "")
	return nil
}

// dispatchResumableJobs launches resumed jobs, capping concurrency at the
// semaphore's size (nil sem = unbounded). It stops early if the service is
// shutting down so it neither leaks while blocked on a full semaphore nor
// starts new work during shutdown.
func (s *Service) dispatchResumableJobs(jobs []*Job, sem chan struct{}) {
	defer s.wg.Done()

	for _, job := range jobs {
		select {
		case <-s.stopped:
			return
		default:
		}

		if sem != nil {
			select {
			case sem <- struct{}{}:
			case <-s.stopped:
				return
			}
		}

		s.mu.Lock()
		if _, exists := s.running[job.JobID]; exists {
			s.mu.Unlock()
			if sem != nil {
				<-sem
			}
			continue
		}
		jobCtx, cancel := context.WithCancelCause(context.Background())
		rj := &runningJob{cancel: cancel}
		s.running[job.JobID] = rj
		s.mu.Unlock()

		s.wg.Add(1)
		go func(tenantID, jobID string) {
			defer func() {
				if sem != nil {
					<-sem
				}
			}()
			s.executeJob(jobCtx, rj, tenantID, jobID)
		}(job.TenantID, job.JobID)
	}
}

func (s *Service) Stop(ctx context.Context) error {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.Stop")
	defer span.End()

	s.stopOnce.Do(func() { close(s.stopped) })

	s.mu.Lock()
	runningJobs := make([]string, 0, len(s.running))
	for jobID, rj := range s.running {
		runningJobs = append(runningJobs, jobID)
		// Not ErrJobCancelled: an interrupted job stays running so the
		// next start resumes it. A job already completing finishes its
		// final write, which does not use this context.
		rj.cancel(errServiceStopping)
	}
	s.running = make(map[string]*runningJob)
	s.mu.Unlock()

	s.logger.InfoContext(ctx, "cancelled running jobs", "count", len(runningJobs))

	// Wait for all jobs to drain with timeout.
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		s.logger.InfoContext(ctx, "all jobs drained")
	case <-ctx.Done():
		s.logger.WarnContext(ctx, "stop timeout, some jobs may still be running")
	}

	span.SetStatus(otelcodes.Ok, "")
	return nil
}

func (s *Service) publishJobState(ctx context.Context, tenantID, jobID string, jobStatus JobStatus) error {
	if s.bus == nil {
		return nil
	}

	msg := map[string]interface{}{
		"tenant_id": tenantID,
		"job_id":    jobID,
		"status":    string(jobStatus),
		"timestamp": time.Now().Unix(),
	}

	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal job state: %w", err)
	}

	subject, err := natsbus.PipelineStateSubject(tenantID, jobID)
	if err != nil {
		return fmt.Errorf("build pipeline state subject: %w", err)
	}

	return s.bus.Publish(ctx, subject, payload)
}

func jobToProto(job *Job, stages []*Stage) *pb.PipelineJob {
	pbStages := make([]*pb.JobStage, len(stages))
	var completedSteps, failedSteps, cancelledSteps int32
	for i, stage := range stages {
		pbStages[i] = stageToProto(stage)
		switch stage.Status {
		case StageStatusCompleted:
			completedSteps++
		case StageStatusFailed:
			failedSteps++
		case StageStatusCancelled:
			cancelledSteps++
		}
	}

	var config pb.JobConfig
	if err := json.Unmarshal(job.Config, &config); err != nil {
		config = pb.JobConfig{}
	}

	totalSteps := int32(len(stages))
	completionPercentage := float32(0)
	if totalSteps > 0 {
		completionPercentage = float32(completedSteps) / float32(totalSteps) * 100.0
	}

	return &pb.PipelineJob{
		JobId:  job.JobID,
		Status: jobStatusToProto(job.Status),
		Config: &config,
		Audit: &pb.AuditMetadata{
			CreatedAt: timestamppb.New(job.CreatedAt),
			UpdatedAt: timestamppb.New(job.UpdatedAt),
			CreatedBy: job.CreatedBy,
		},
		Progress: &pb.JobProgress{
			TotalSteps:           totalSteps,
			CompletedSteps:       completedSteps,
			FailedSteps:          failedSteps,
			CancelledSteps:       cancelledSteps,
			CompletionPercentage: completionPercentage,
		},
		Stages: pbStages,
	}
}

func stageToProto(stage *Stage) *pb.JobStage {
	pbStage := &pb.JobStage{
		StageName:  stage.StageName,
		Status:     stageStatusToProtoJobStatus(stage.Status),
		RetryCount: int32(stage.RetryCount),
	}

	if stage.StartedAt != nil {
		pbStage.StartedAt = timestamppb.New(*stage.StartedAt)
	}
	if stage.CompletedAt != nil {
		pbStage.CompletedAt = timestamppb.New(*stage.CompletedAt)
	}
	if stage.ErrorMessage != "" {
		var pbErr pb.Error
		pbErr.Message = stage.ErrorMessage
		pbStage.Error = &pbErr
	}

	return pbStage
}

func jobStatusToProto(js JobStatus) pb.JobStatus {
	switch js {
	case JobStatusPending:
		return pb.JobStatus_JOB_STATUS_PENDING
	case JobStatusRunning:
		return pb.JobStatus_JOB_STATUS_RUNNING
	case JobStatusCompleted:
		return pb.JobStatus_JOB_STATUS_COMPLETED
	case JobStatusFailed:
		return pb.JobStatus_JOB_STATUS_FAILED
	case JobStatusCancelled:
		return pb.JobStatus_JOB_STATUS_CANCELLED
	default:
		return pb.JobStatus_JOB_STATUS_UNSPECIFIED
	}
}

func jobStatusFromProto(pbStatus pb.JobStatus) JobStatus {
	switch pbStatus {
	case pb.JobStatus_JOB_STATUS_PENDING:
		return JobStatusPending
	case pb.JobStatus_JOB_STATUS_RUNNING:
		return JobStatusRunning
	case pb.JobStatus_JOB_STATUS_COMPLETED:
		return JobStatusCompleted
	case pb.JobStatus_JOB_STATUS_FAILED:
		return JobStatusFailed
	case pb.JobStatus_JOB_STATUS_CANCELLED:
		return JobStatusCancelled
	default:
		return ""
	}
}

func stageStatusToProtoJobStatus(ss StageStatus) pb.JobStatus {
	switch ss {
	case StageStatusPending:
		return pb.JobStatus_JOB_STATUS_PENDING
	case StageStatusRunning:
		return pb.JobStatus_JOB_STATUS_RUNNING
	case StageStatusCompleted:
		return pb.JobStatus_JOB_STATUS_COMPLETED
	case StageStatusFailed:
		return pb.JobStatus_JOB_STATUS_FAILED
	case StageStatusCancelled:
		return pb.JobStatus_JOB_STATUS_CANCELLED
	default:
		return pb.JobStatus_JOB_STATUS_UNSPECIFIED
	}
}

// reserveRetry claims job for one RetryJob call and returns the function
// that releases it. It refuses, with FailedPrecondition, a job this service
// is still running or still stopping (CancelJob returned, but the run is
// still recording the cancellation), and a job another RetryJob call is
// acting on, so exactly one of concurrent retries proceeds and no retry
// starts a second run of a job beside the first.
func (s *Service) reserveRetry(job *Job) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rj, ok := s.running[job.JobID]; ok {
		if rj.cancelled {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
				"job %s is still stopping; retry it when \"crosscodex run status %s\" shows it is no longer running",
				job.JobID, job.JobID))
		}
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"job %s is running; a running job cannot be retried. Wait for it to end, or cancel it with "+
				"\"crosscodex run cancel --yes %s\" and retry once \"crosscodex run status %s\" shows it is no longer running",
			job.JobID, job.JobID, job.JobID))
	}
	if s.retrying[job.JobID] {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"job %s is already being retried; check \"crosscodex run status %s\"", job.JobID, job.JobID))
	}
	s.retrying[job.JobID] = true
	return func() {
		s.mu.Lock()
		delete(s.retrying, job.JobID)
		s.mu.Unlock()
	}, nil
}

// retryJobConfig is the config of the new job RetryJob creates from job: its
// stored config, validated as CreateJob validates a request's. A config
// stored before target_catalog_id ran within its one catalog, so the new
// job's config states that catalog as its target. A stored config that sets
// bidirectional within one catalog runs without it on resume
// (jobconfig.ExtractCatalogs), so the new job's config drops it too. The
// caller records the new job's catalog snapshot.
func retryJobConfig(job *Job) (*pb.JobConfig, error) {
	var cfg pb.JobConfig
	if err := json.Unmarshal(job.Config, &cfg); err != nil {
		return nil, fmt.Errorf("cannot retry job %s as a new job: its stored config could not be read as a job config. "+
			"Start a new job instead: %s", job.JobID, newJobCommand(job))
	}
	if cfg.GetTargetCatalogId() == "" {
		cfg.TargetCatalogId = cfg.GetCatalogId()
	}
	if cfg.GetTargetCatalogId() == cfg.GetCatalogId() {
		cfg.Bidirectional = false
	}
	if err := validateJobConfig(&cfg); err != nil {
		return nil, fmt.Errorf("cannot retry job %s as a new job: its stored config is invalid: %w. "+
			"Start a new job instead: %s", job.JobID, err, newJobCommand(job))
	}
	return &cfg, nil
}
