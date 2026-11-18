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

func (s *Service) SubmitVote(ctx context.Context, req *connect.Request[pb.SubmitVoteRequest]) (*connect.Response[pb.SubmitVoteResponse], error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}

	ctx, endSpan := s.startHandlerSpan(ctx, "SubmitVote", identity,
		attribute.String("mapping.id", req.Msg.GetMappingId()),
		attribute.String("vote.type", req.Msg.GetVoteType().String()))
	defer endSpan()

	if req.Msg.GetMappingId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("mapping_id is required"))
	}

	if s.feedback == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("feedback backend not configured"))
	}

	tc := buildTenantContext(identity)
	req.Msg.TenantContext = tc

	resp, err := s.feedback.SubmitVote(ctx, req)
	if err != nil {
		err = s.publicError(ctx, "SubmitVote", err)
		s.recordMetrics(ctx, "SubmitVote", start, connect.CodeOf(err))
		return nil, err
	}

	s.recordMetrics(ctx, "SubmitVote", start, connect.Code(0))
	return resp, nil
}

func (s *Service) GetReviewQueue(ctx context.Context, req *connect.Request[pb.GetReviewQueueRequest]) (*connect.Response[pb.GetReviewQueueResponse], error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}

	// Admin-only endpoint
	if err := authn.RequireRole(*identity, authn.RoleAdmin); err != nil {
		s.recordMetrics(ctx, "GetReviewQueue", start, connect.CodePermissionDenied)
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("admin access required"))
	}

	ctx, endSpan := s.startHandlerSpan(ctx, "GetReviewQueue", identity)
	defer endSpan()

	if s.feedback == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("feedback backend not configured"))
	}

	tc := buildTenantContext(identity)
	req.Msg.TenantContext = tc

	resp, err := s.feedback.GetReviewQueue(ctx, req)
	if err != nil {
		err = s.publicError(ctx, "GetReviewQueue", err)
		s.recordMetrics(ctx, "GetReviewQueue", start, connect.CodeOf(err))
		return nil, err
	}

	s.recordMetrics(ctx, "GetReviewQueue", start, connect.Code(0))
	return resp, nil
}
