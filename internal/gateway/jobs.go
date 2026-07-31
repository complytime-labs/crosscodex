package gateway

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"go.opentelemetry.io/otel/attribute"
)

func (s *Service) GetJob(ctx context.Context, req *connect.Request[pb.GetJobRequest]) (*connect.Response[pb.GetJobResponse], error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}

	ctx, endSpan := s.startHandlerSpan(ctx, "GetJob", identity,
		attribute.String("job.id", req.Msg.GetJobId()))
	defer endSpan()

	if req.Msg.GetJobId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("job_id is required"))
	}

	if s.pipeline == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("pipeline backend not configured"))
	}

	tc := buildTenantContext(identity)
	req.Msg.TenantContext = tc

	resp, err := s.pipeline.GetJob(ctx, req)
	if err != nil {
		s.recordMetrics(ctx, "GetJob", start, connect.CodeOf(err))
		return nil, err
	}

	if resp.Msg.GetJob() == nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("backend returned nil job"))
	}

	if !authn.IsAdmin(*identity) && resp.Msg.GetJob().GetAudit().GetCreatedBy() != identity.Subject {
		s.recordMetrics(ctx, "GetJob", start, connect.CodePermissionDenied)
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("not the job owner"))
	}

	s.recordMetrics(ctx, "GetJob", start, connect.Code(0))
	return resp, nil
}

func (s *Service) ListJobs(ctx context.Context, req *connect.Request[pb.ListJobsRequest]) (*connect.Response[pb.ListJobsResponse], error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}

	ctx, endSpan := s.startHandlerSpan(ctx, "ListJobs", identity)
	defer endSpan()

	if s.pipeline == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("pipeline backend not configured"))
	}

	tc := buildTenantContext(identity)
	req.Msg.TenantContext = tc

	resp, err := s.pipeline.ListJobs(ctx, req)
	if err != nil {
		s.recordMetrics(ctx, "ListJobs", start, connect.CodeOf(err))
		return nil, err
	}

	if !authn.IsAdmin(*identity) {
		filtered := make([]*pb.PipelineJob, 0, len(resp.Msg.GetJobs()))
		for _, job := range resp.Msg.GetJobs() {
			if job.GetAudit().GetCreatedBy() == identity.Subject {
				filtered = append(filtered, job)
			}
		}
		resp.Msg.Jobs = filtered
	}

	s.recordMetrics(ctx, "ListJobs", start, connect.Code(0))
	return resp, nil
}

func (s *Service) CancelJob(ctx context.Context, req *connect.Request[pb.CancelJobRequest]) (*connect.Response[pb.CancelJobResponse], error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}

	ctx, endSpan := s.startHandlerSpan(ctx, "CancelJob", identity,
		attribute.String("job.id", req.Msg.GetJobId()))
	defer endSpan()

	if req.Msg.GetJobId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("job_id is required"))
	}

	if s.pipeline == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("pipeline backend not configured"))
	}

	tc := buildTenantContext(identity)

	if !authn.IsAdmin(*identity) {
		getResp, err := s.pipeline.GetJob(ctx, connect.NewRequest(&pb.GetJobRequest{
			TenantContext: tc,
			JobId:         req.Msg.GetJobId(),
		}))
		if err != nil {
			s.recordMetrics(ctx, "CancelJob", start, connect.CodeOf(err))
			return nil, err
		}
		if getResp.Msg.GetJob() == nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("backend returned nil job"))
		}
		if getResp.Msg.GetJob().GetAudit().GetCreatedBy() != identity.Subject {
			s.recordMetrics(ctx, "CancelJob", start, connect.CodePermissionDenied)
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("not the job owner"))
		}
	}

	req.Msg.TenantContext = tc

	resp, err := s.pipeline.CancelJob(ctx, req)
	if err != nil {
		s.recordMetrics(ctx, "CancelJob", start, connect.CodeOf(err))
		return nil, err
	}

	s.recordMetrics(ctx, "CancelJob", start, connect.Code(0))
	return resp, nil
}
