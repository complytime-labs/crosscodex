package gateway

import (
	"context"
	"time"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Service) GetJob(ctx context.Context, req *pb.GetJobRequest) (*pb.GetJobResponse, error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, status.Error(codes.Unauthenticated, "not authenticated")
	}

	if s.tracer != nil {
		var span trace.Span
		ctx, span = s.tracer.Start(ctx, "gateway.GetJob",
			trace.WithAttributes(
				attribute.String("rpc.method", "GetJob"),
				attribute.String("tenant.id", identity.TenantID),
				attribute.String("user.id", identity.Subject),
				attribute.String("job.id", req.GetJobId()),
			))
		defer span.End()
	}

	if req.GetJobId() == "" {
		return nil, status.Error(codes.InvalidArgument, "job_id is required")
	}

	if s.pipeline == nil {
		return nil, status.Error(codes.Unavailable, "pipeline backend not configured")
	}

	tc := buildTenantContext(identity)
	req.TenantContext = tc

	resp, err := s.pipeline.GetJob(ctx, req)
	if err != nil {
		s.recordMetrics(ctx, "GetJob", start, status.Code(err))
		return nil, err
	}

	if resp.GetJob() == nil {
		return nil, status.Error(codes.Internal, "backend returned nil job")
	}

	if !authn.IsAdmin(*identity) && resp.GetJob().GetAudit().GetCreatedBy() != identity.Subject {
		s.recordMetrics(ctx, "GetJob", start, codes.PermissionDenied)
		return nil, status.Error(codes.PermissionDenied, "not the job owner")
	}

	s.recordMetrics(ctx, "GetJob", start, codes.OK)
	return resp, nil
}

func (s *Service) ListJobs(ctx context.Context, req *pb.ListJobsRequest) (*pb.ListJobsResponse, error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, status.Error(codes.Unauthenticated, "not authenticated")
	}

	if s.tracer != nil {
		var span trace.Span
		ctx, span = s.tracer.Start(ctx, "gateway.ListJobs",
			trace.WithAttributes(
				attribute.String("rpc.method", "ListJobs"),
				attribute.String("tenant.id", identity.TenantID),
				attribute.String("user.id", identity.Subject),
			))
		defer span.End()
	}

	if s.pipeline == nil {
		return nil, status.Error(codes.Unavailable, "pipeline backend not configured")
	}

	tc := buildTenantContext(identity)
	req.TenantContext = tc

	resp, err := s.pipeline.ListJobs(ctx, req)
	if err != nil {
		s.recordMetrics(ctx, "ListJobs", start, status.Code(err))
		return nil, err
	}

	if !authn.IsAdmin(*identity) {
		filtered := make([]*pb.PipelineJob, 0, len(resp.GetJobs()))
		for _, job := range resp.GetJobs() {
			if job.GetAudit().GetCreatedBy() == identity.Subject {
				filtered = append(filtered, job)
			}
		}
		resp.Jobs = filtered
	}

	s.recordMetrics(ctx, "ListJobs", start, codes.OK)
	return resp, nil
}

func (s *Service) CancelJob(ctx context.Context, req *pb.CancelJobRequest) (*pb.CancelJobResponse, error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, status.Error(codes.Unauthenticated, "not authenticated")
	}

	if s.tracer != nil {
		var span trace.Span
		ctx, span = s.tracer.Start(ctx, "gateway.CancelJob",
			trace.WithAttributes(
				attribute.String("rpc.method", "CancelJob"),
				attribute.String("tenant.id", identity.TenantID),
				attribute.String("user.id", identity.Subject),
				attribute.String("job.id", req.GetJobId()),
			))
		defer span.End()
	}

	if req.GetJobId() == "" {
		return nil, status.Error(codes.InvalidArgument, "job_id is required")
	}

	if s.pipeline == nil {
		return nil, status.Error(codes.Unavailable, "pipeline backend not configured")
	}

	tc := buildTenantContext(identity)

	if !authn.IsAdmin(*identity) {
		getResp, err := s.pipeline.GetJob(ctx, &pb.GetJobRequest{
			TenantContext: tc,
			JobId:         req.GetJobId(),
		})
		if err != nil {
			s.recordMetrics(ctx, "CancelJob", start, status.Code(err))
			return nil, err
		}
		if getResp.GetJob() == nil {
			return nil, status.Error(codes.Internal, "backend returned nil job")
		}
		if getResp.GetJob().GetAudit().GetCreatedBy() != identity.Subject {
			s.recordMetrics(ctx, "CancelJob", start, codes.PermissionDenied)
			return nil, status.Error(codes.PermissionDenied, "not the job owner")
		}
	}

	req.TenantContext = tc

	resp, err := s.pipeline.CancelJob(ctx, req)
	if err != nil {
		s.recordMetrics(ctx, "CancelJob", start, status.Code(err))
		return nil, err
	}

	s.recordMetrics(ctx, "CancelJob", start, codes.OK)
	return resp, nil
}
