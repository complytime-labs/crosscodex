package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func (s *Service) checkConcurrencyLimit() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	runningCount := len(s.running)
	if s.cfg.MaxConcurrentJobs > 0 && runningCount >= s.cfg.MaxConcurrentJobs {
		return status.Errorf(codes.ResourceExhausted, "max concurrent jobs reached (%d)", s.cfg.MaxConcurrentJobs)
	}
	return nil
}

func (s *Service) CreateJob(ctx context.Context, req *pb.CreateJobRequest) (*pb.CreateJobResponse, error) {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.CreateJob")
	defer span.End()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, status.Error(codes.InvalidArgument, "missing tenant context")
	}

	span.SetAttributes(attribute.String("tenant.id", tenantID))

	if req.Config == nil {
		return nil, status.Error(codes.InvalidArgument, "config is required")
	}

	dag, err := s.registry.BuildDAG(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, status.Errorf(codes.Internal, "build DAG: %v", err)
	}

	jobID := uuid.New().String()

	configBytes, err := json.Marshal(req.Config)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, status.Errorf(codes.InvalidArgument, "marshal config: %v", err)
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
		CreatedBy: tenantID,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := s.store.CreateJob(ctx, job); err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, status.Errorf(codes.Internal, "create job: %v", err)
	}

	dagOrder := dag.Order()
	stageNames := make([]string, 0, len(dagOrder)+2)
	stageNames = append(stageNames, dagOrder...)
	stageNames = append(stageNames, "synthesis", "graph")

	if err := s.store.CreateStages(ctx, jobID, stageNames); err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, status.Errorf(codes.Internal, "create stages: %v", err)
	}

	// Register cancel func after successful DB creation.
	s.mu.Lock()
	jobCtx, cancel := context.WithCancel(context.Background())
	s.running[jobID] = cancel
	s.mu.Unlock()

	s.wg.Add(1)
	go s.executeJob(jobCtx, tenantID, jobID)

	if err := s.publishJobState(ctx, tenantID, jobID, JobStatusPending); err != nil {
		s.logger.WarnContext(ctx, "failed to publish job state", "job_id", jobID, "error", err)
	}

	span.SetStatus(otelcodes.Ok, "")
	return &pb.CreateJobResponse{
		JobId:  jobID,
		Status: jobStatusToProto(JobStatusPending),
	}, nil
}

func (s *Service) GetJob(ctx context.Context, req *pb.GetJobRequest) (*pb.GetJobResponse, error) {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.GetJob")
	defer span.End()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, status.Error(codes.InvalidArgument, "missing tenant context")
	}

	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.String("job.id", req.JobId),
	)

	job, err := s.store.GetJob(ctx, req.JobId)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, status.Error(codes.NotFound, "job not found")
		}
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, status.Errorf(codes.Internal, "get job: %v", err)
	}

	stages, err := s.store.GetStages(ctx, req.JobId)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, status.Errorf(codes.Internal, "get stages: %v", err)
	}

	pbJob := jobToProto(job, stages)

	span.SetStatus(otelcodes.Ok, "")
	return &pb.GetJobResponse{Job: pbJob}, nil
}

func (s *Service) ListJobs(ctx context.Context, req *pb.ListJobsRequest) (*pb.ListJobsResponse, error) {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.ListJobs")
	defer span.End()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, status.Error(codes.InvalidArgument, "missing tenant context")
	}

	span.SetAttributes(attribute.String("tenant.id", tenantID))

	var pageSize int32 = 50
	var pageToken string
	if req.Options != nil && req.Options.Pagination != nil {
		if req.Options.Pagination.PageSize > 0 {
			pageSize = req.Options.Pagination.PageSize
		}
		pageToken = req.Options.Pagination.PageToken
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
	if req.Status != pb.JobStatus_JOB_STATUS_UNSPECIFIED {
		filter.Status = jobStatusFromProto(req.Status)
	}

	jobs, total, err := s.store.ListJobs(ctx, tenantID, filter)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, status.Errorf(codes.Internal, "list jobs: %v", err)
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
	return &pb.ListJobsResponse{
		Jobs: pbJobs,
		PageInfo: &pb.PageInfo{
			NextPageToken: nextPageToken,
			TotalCount:    total,
		},
	}, nil
}

func (s *Service) CancelJob(ctx context.Context, req *pb.CancelJobRequest) (*pb.CancelJobResponse, error) {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.CancelJob")
	defer span.End()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, status.Error(codes.InvalidArgument, "missing tenant context")
	}

	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.String("job.id", req.JobId),
	)

	if _, err := s.store.GetJob(ctx, req.JobId); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, status.Error(codes.NotFound, "job not found")
		}
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, status.Errorf(codes.Internal, "get job: %v", err)
	}

	s.mu.Lock()
	cancel, ok := s.running[req.JobId]
	if !ok {
		s.mu.Unlock()
		return nil, status.Error(codes.NotFound, "job is not running")
	}
	cancel()
	delete(s.running, req.JobId)
	s.mu.Unlock()

	// Status update and NATS event handled by executeJob's ctx.Err() path.

	span.SetStatus(otelcodes.Ok, "")
	return &pb.CancelJobResponse{
		Cancelled: true,
	}, nil
}

func (s *Service) RetryJob(ctx context.Context, req *pb.RetryJobRequest) (*pb.RetryJobResponse, error) {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.RetryJob")
	defer span.End()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, status.Error(codes.InvalidArgument, "missing tenant context")
	}

	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.String("job.id", req.JobId),
	)

	job, err := s.store.GetJob(ctx, req.JobId)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, status.Error(codes.NotFound, "job not found")
		}
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, status.Errorf(codes.Internal, "get job: %v", err)
	}

	// Check concurrency limit before spawning retry job.
	if err := s.checkConcurrencyLimit(); err != nil {
		return nil, err
	}

	var newJobID string

	if req.RetryFromFailure {
		stages, err := s.store.GetStages(ctx, req.JobId)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(otelcodes.Error, err.Error())
			return nil, status.Errorf(codes.Internal, "get stages: %v", err)
		}

		var firstFailed string
		stageNames := make([]string, len(stages))
		for i, stage := range stages {
			stageNames[i] = stage.StageName
			if firstFailed == "" && stage.Status == StageStatusFailed {
				firstFailed = stage.StageName
			}
		}

		if firstFailed == "" {
			return nil, status.Error(codes.FailedPrecondition, "no failed stage to retry from")
		}

		if err := s.store.ResetStagesFrom(ctx, req.JobId, firstFailed, stageNames); err != nil {
			span.RecordError(err)
			span.SetStatus(otelcodes.Error, err.Error())
			return nil, status.Errorf(codes.Internal, "reset stages: %v", err)
		}

		if err := s.store.UpdateJobStatus(ctx, req.JobId, JobStatusRunning, nil); err != nil {
			span.RecordError(err)
			span.SetStatus(otelcodes.Error, err.Error())
			return nil, status.Errorf(codes.Internal, "update job status: %v", err)
		}

		s.mu.Lock()
		jobCtx, cancel := context.WithCancel(context.Background())
		s.running[req.JobId] = cancel
		s.mu.Unlock()

		s.wg.Add(1)
		go s.executeJob(jobCtx, tenantID, req.JobId)

		newJobID = req.JobId
	} else {
		newJobID = uuid.New().String()

		now := time.Now()
		newJob := &Job{
			JobID:     newJobID,
			TenantID:  tenantID,
			Status:    JobStatusPending,
			Config:    job.Config,
			CreatedBy: tenantID,
			CreatedAt: now,
			UpdatedAt: now,
		}

		if err := s.store.CreateJob(ctx, newJob); err != nil {
			span.RecordError(err)
			span.SetStatus(otelcodes.Error, err.Error())
			return nil, status.Errorf(codes.Internal, "create job: %v", err)
		}

		dag, err := s.registry.BuildDAG(ctx)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(otelcodes.Error, err.Error())
			return nil, status.Errorf(codes.Internal, "build DAG: %v", err)
		}

		dagOrder := dag.Order()
		stageNames := make([]string, 0, len(dagOrder)+2)
		stageNames = append(stageNames, dagOrder...)
		stageNames = append(stageNames, "synthesis", "graph")

		if err := s.store.CreateStages(ctx, newJobID, stageNames); err != nil {
			span.RecordError(err)
			span.SetStatus(otelcodes.Error, err.Error())
			return nil, status.Errorf(codes.Internal, "create stages: %v", err)
		}

		s.mu.Lock()
		jobCtx, cancel := context.WithCancel(context.Background())
		s.running[newJobID] = cancel
		s.mu.Unlock()

		s.wg.Add(1)
		go s.executeJob(jobCtx, tenantID, newJobID)
	}

	if err := s.publishJobState(ctx, tenantID, newJobID, JobStatusRunning); err != nil {
		s.logger.WarnContext(ctx, "failed to publish job state", "job_id", newJobID, "error", err)
	}

	span.SetStatus(otelcodes.Ok, "")
	return &pb.RetryJobResponse{
		NewJobId: newJobID,
	}, nil
}

func (s *Service) GetJobTrace(ctx context.Context, req *pb.GetJobTraceRequest) (*pb.GetJobTraceResponse, error) {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.GetJobTrace")
	defer span.End()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, status.Error(codes.InvalidArgument, "missing tenant context")
	}

	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.String("job.id", req.JobId),
	)

	span.SetStatus(otelcodes.Ok, "")
	return nil, status.Error(codes.Unimplemented, "GetJobTrace is not yet implemented")
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

	for _, job := range jobs {
		s.mu.Lock()
		if _, exists := s.running[job.JobID]; exists {
			s.mu.Unlock()
			continue
		}

		jobCtx, cancel := context.WithCancel(context.Background())
		s.running[job.JobID] = cancel
		s.mu.Unlock()

		s.wg.Add(1)
		go s.executeJob(jobCtx, job.TenantID, job.JobID)
	}

	span.SetStatus(otelcodes.Ok, "")
	return nil
}

func (s *Service) Stop(ctx context.Context) error {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.Stop")
	defer span.End()

	s.mu.Lock()
	runningJobs := make([]string, 0, len(s.running))
	for jobID, cancel := range s.running {
		runningJobs = append(runningJobs, jobID)
		cancel()
	}
	s.running = make(map[string]context.CancelFunc)
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
	var completedSteps, failedSteps int32
	for i, stage := range stages {
		pbStages[i] = stageToProto(stage)
		switch stage.Status {
		case StageStatusCompleted:
			completedSteps++
		case StageStatusFailed:
			failedSteps++
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
	default:
		return pb.JobStatus_JOB_STATUS_UNSPECIFIED
	}
}
