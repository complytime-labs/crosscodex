package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"go.opentelemetry.io/otel/attribute"
)

// maxControlMappingsLimit is the largest page the mappings backend serves
// (internal/results MaxPageSize); GetControlMappings rejects more up front.
const maxControlMappingsLimit = 1000

func (s *Service) ListJobMappings(ctx context.Context, req *connect.Request[pb.ListJobMappingsRequest]) (*connect.Response[pb.ListJobMappingsResponse], error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}

	ctx, endSpan := s.startHandlerSpan(ctx, "ListJobMappings", identity,
		attribute.String("job.id", req.Msg.GetJobId()))
	defer endSpan()

	if req.Msg.GetJobId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("job_id is required"))
	}

	if s.mappings == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("mappings backend not configured"))
	}

	req.Msg.TenantContext = buildTenantContext(identity)

	resp, err := s.mappings.ListMappings(ctx, req)
	if err != nil {
		err = s.publicError(ctx, "ListJobMappings", err)
		s.recordMetrics(ctx, "ListJobMappings", start, connect.CodeOf(err))
		return nil, err
	}

	s.recordMetrics(ctx, "ListJobMappings", start, connect.Code(0))
	return resp, nil
}

// GetControlMappings lists the synthesized mappings of one control across all
// of the tenant's jobs.
func (s *Service) GetControlMappings(ctx context.Context, req *connect.Request[pb.GetControlMappingsRequest]) (*connect.Response[pb.GetControlMappingsResponse], error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}

	ctx, endSpan := s.startHandlerSpan(ctx, "GetControlMappings", identity,
		attribute.String("control.id", req.Msg.GetControlId()))
	defer endSpan()

	if req.Msg.GetControlId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("control_id is required"))
	}

	if limit := req.Msg.GetLimit(); limit < 0 || limit > maxControlMappingsLimit {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
			"limit must be between 0 and %d (0 = default page); got %d. To read more mappings of a control, page through ListJobMappings", maxControlMappingsLimit, limit))
	}

	if s.mappings == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("mappings backend not configured"))
	}

	listReq := &pb.ListJobMappingsRequest{
		TenantContext: buildTenantContext(identity),
		ControlId:     req.Msg.GetControlId(),
		MinConfidence: req.Msg.GetMinConfidence(),
		Stage:         pb.MappingStage_MAPPING_STAGE_SYNTHESIZED,
	}
	if limit := req.Msg.GetLimit(); limit > 0 {
		listReq.Pagination = &pb.Pagination{PageSize: limit}
	}

	resp, err := s.mappings.ListMappings(ctx, connect.NewRequest(listReq))
	if err != nil {
		err = s.publicError(ctx, "GetControlMappings", err)
		s.recordMetrics(ctx, "GetControlMappings", start, connect.CodeOf(err))
		return nil, err
	}

	s.recordMetrics(ctx, "GetControlMappings", start, connect.Code(0))

	// GetControlMappingsRequest has no page_token field, so a next token
	// could never be redeemed; clear it and keep the rest of the page info.
	var pageInfo *pb.PageInfo
	if backendInfo := resp.Msg.GetPageInfo(); backendInfo != nil {
		pageInfo = &pb.PageInfo{TotalCount: backendInfo.GetTotalCount()}
	}
	return connect.NewResponse(&pb.GetControlMappingsResponse{
		Mappings: resp.Msg.GetMappings(),
		PageInfo: pageInfo,
	}), nil
}
