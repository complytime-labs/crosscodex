package gateway

import (
	"context"
	"fmt"
	"time"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Service) SubmitVote(ctx context.Context, req *pb.SubmitVoteRequest) (*pb.SubmitVoteResponse, error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, status.Error(codes.Unauthenticated, "not authenticated")
	}

	if s.tracer != nil {
		var span trace.Span
		ctx, span = s.tracer.Start(ctx, "gateway.SubmitVote",
			trace.WithAttributes(
				attribute.String("rpc.method", "SubmitVote"),
				attribute.String("tenant.id", identity.TenantID),
				attribute.String("user.id", identity.Subject),
				attribute.String("mapping.id", req.GetMappingId()),
				attribute.String("vote.type", req.GetVoteType().String()),
			))
		defer span.End()
	}

	if req.GetMappingId() == "" {
		return nil, status.Error(codes.InvalidArgument, "mapping_id is required")
	}

	if s.feedback == nil {
		return nil, status.Error(codes.Unavailable, "feedback backend not configured")
	}

	tc := buildTenantContext(identity)
	req.TenantContext = tc

	resp, err := s.feedback.SubmitVote(ctx, req)
	if err != nil {
		s.recordMetrics(ctx, "SubmitVote", start, status.Code(err))
		return nil, err
	}

	// Attestation: emit link for human feedback event
	if s.attestor != nil {
		materials := []attestation.Artifact{
			{URI: fmt.Sprintf("mapping://%s/%s", identity.TenantID, req.GetMappingId()), Digest: ""},
		}
		products := []attestation.Artifact{
			{URI: fmt.Sprintf("vote://%s/%s", identity.TenantID, resp.GetVoteId()), Digest: ""},
		}

		traceID := telemetry.TraceIDFromContext(ctx)
		byProducts := map[string]any{
			"vote_type": req.GetVoteType().String(),
			"user_id":   identity.Subject,
		}
		if req.GetSuggestedType() != pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED {
			byProducts["suggested_type"] = req.GetSuggestedType().String()
		}
		if req.GetRationale() != "" {
			byProducts["rationale_length"] = len(req.GetRationale())
		}

		link, err := s.attestor.CreateLink(ctx, "gateway.SubmitVote", materials, products, attestation.WithByProducts(byProducts))
		if err != nil {
			// Log but don't fail the request
			if s.tracer != nil {
				trace.SpanFromContext(ctx).RecordError(err, trace.WithAttributes(
					attribute.String("error.type", "attestation_failed"),
					attribute.String("trace_id", traceID),
				))
			}
		} else {
			_ = link // Successfully created, stored by attestor
		}
	}

	s.recordMetrics(ctx, "SubmitVote", start, codes.OK)
	return resp, nil
}

func (s *Service) GetReviewQueue(ctx context.Context, req *pb.GetReviewQueueRequest) (*pb.GetReviewQueueResponse, error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, status.Error(codes.Unauthenticated, "not authenticated")
	}

	// Admin-only endpoint
	if err := authn.RequireRole(*identity, authn.RoleAdmin); err != nil {
		s.recordMetrics(ctx, "GetReviewQueue", start, codes.PermissionDenied)
		return nil, status.Error(codes.PermissionDenied, "admin access required")
	}

	if s.tracer != nil {
		var span trace.Span
		ctx, span = s.tracer.Start(ctx, "gateway.GetReviewQueue",
			trace.WithAttributes(
				attribute.String("rpc.method", "GetReviewQueue"),
				attribute.String("tenant.id", identity.TenantID),
				attribute.String("user.id", identity.Subject),
			))
		defer span.End()
	}

	if s.feedback == nil {
		return nil, status.Error(codes.Unavailable, "feedback backend not configured")
	}

	tc := buildTenantContext(identity)
	req.TenantContext = tc

	resp, err := s.feedback.GetReviewQueue(ctx, req)
	if err != nil {
		s.recordMetrics(ctx, "GetReviewQueue", start, status.Code(err))
		return nil, err
	}

	s.recordMetrics(ctx, "GetReviewQueue", start, codes.OK)
	return resp, nil
}
