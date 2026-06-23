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

func (s *Service) GetControlMappings(ctx context.Context, req *pb.GetControlMappingsRequest) (*pb.GetControlMappingsResponse, error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, status.Error(codes.Unauthenticated, "not authenticated")
	}

	if s.tracer != nil {
		var span trace.Span
		ctx, span = s.tracer.Start(ctx, "gateway.GetControlMappings",
			trace.WithAttributes(
				attribute.String("rpc.method", "GetControlMappings"),
				attribute.String("tenant.id", identity.TenantID),
				attribute.String("user.id", identity.Subject),
				attribute.String("control.id", req.GetControlId()),
			))
		defer span.End()
	}

	if req.GetControlId() == "" {
		return nil, status.Error(codes.InvalidArgument, "control_id is required")
	}

	if s.graph == nil {
		return nil, status.Error(codes.Unavailable, "graph backend not configured")
	}

	tc := buildTenantContext(identity)

	// Translate to TraverseRequest
	traverseReq := &pb.TraverseRequest{
		TenantContext: tc,
		StartNodeId:   req.GetControlId(),
		Direction:     pb.TraversalDirection_TRAVERSAL_DIRECTION_OUTBOUND,
		EdgeLabels:    []string{"maps_to"},
		MaxDepth:      1,
	}

	if req.GetLimit() > 0 {
		traverseReq.Options = &pb.ListOptions{
			Pagination: &pb.Pagination{
				PageSize: req.GetLimit(),
			},
		}
	}

	traverseResp, err := s.graph.Traverse(ctx, traverseReq)
	if err != nil {
		s.recordMetrics(ctx, "GetControlMappings", start, status.Code(err))
		return nil, err
	}

	// Convert edges to ControlMappings
	mappings := make([]*pb.ControlMapping, 0, len(traverseResp.GetEdges()))
	for _, edge := range traverseResp.GetEdges() {
		if edge.GetLabel() != "maps_to" {
			continue
		}

		mapping := &pb.ControlMapping{
			MappingId:        edge.GetEdgeId(),
			SourceControlId:  edge.GetSourceNodeId(),
			TargetControlId:  edge.GetTargetNodeId(),
			RelationshipType: edge.GetRelationshipType(),
			Confidence:       1.0, // Graph edges are confirmed
			IsViable:         true,
		}

		// Filter by min_confidence
		if req.GetMinConfidence() > 0 && mapping.Confidence < req.GetMinConfidence() {
			continue
		}

		mappings = append(mappings, mapping)
	}

	s.recordMetrics(ctx, "GetControlMappings", start, codes.OK)

	return &pb.GetControlMappingsResponse{
		Mappings: mappings,
		PageInfo: traverseResp.GetPageInfo(),
	}, nil
}

func (s *Service) QueryGraph(ctx context.Context, req *pb.QueryGraphRequest) (*pb.QueryGraphResponse, error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, status.Error(codes.Unauthenticated, "not authenticated")
	}

	// Admin-only endpoint
	if err := authn.RequireRole(*identity, authn.RoleAdmin); err != nil {
		s.recordMetrics(ctx, "QueryGraph", start, codes.PermissionDenied)
		return nil, status.Error(codes.PermissionDenied, "admin access required")
	}

	if s.tracer != nil {
		var span trace.Span
		ctx, span = s.tracer.Start(ctx, "gateway.QueryGraph",
			trace.WithAttributes(
				attribute.String("rpc.method", "QueryGraph"),
				attribute.String("tenant.id", identity.TenantID),
				attribute.String("user.id", identity.Subject),
			))
		defer span.End()
	}

	if req.GetCypher() == "" {
		return nil, status.Error(codes.InvalidArgument, "cypher is required")
	}

	if s.graph == nil {
		return nil, status.Error(codes.Unavailable, "graph backend not configured")
	}

	tc := buildTenantContext(identity)

	// Delegate to graph backend's Query
	queryReq := &pb.QueryRequest{
		TenantContext: tc,
		Cypher:        req.GetCypher(),
		Parameters:    req.GetParameters(),
		AsOf:          req.GetAsOf(),
	}

	queryResp, err := s.graph.Query(ctx, queryReq)
	if err != nil {
		s.recordMetrics(ctx, "QueryGraph", start, status.Code(err))
		return nil, err
	}

	s.recordMetrics(ctx, "QueryGraph", start, codes.OK)

	return &pb.QueryGraphResponse{
		Response: queryResp,
	}, nil
}

func (s *Service) FindSimilar(ctx context.Context, req *pb.FindSimilarRequest) (*pb.FindSimilarResponse, error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, status.Error(codes.Unauthenticated, "not authenticated")
	}

	if s.tracer != nil {
		var span trace.Span
		ctx, span = s.tracer.Start(ctx, "gateway.FindSimilar",
			trace.WithAttributes(
				attribute.String("rpc.method", "FindSimilar"),
				attribute.String("tenant.id", identity.TenantID),
				attribute.String("user.id", identity.Subject),
				attribute.String("control.id", req.GetControlId()),
			))
		defer span.End()
	}

	if req.GetControlId() == "" {
		return nil, status.Error(codes.InvalidArgument, "control_id is required")
	}

	if s.graph == nil {
		return nil, status.Error(codes.Unavailable, "graph backend not configured")
	}

	tc := buildTenantContext(identity)

	// Translate to SimilaritySearchRequest
	searchReq := &pb.SimilaritySearchRequest{
		TenantContext: tc,
		Query:         &pb.SimilaritySearchRequest_ControlId{ControlId: req.GetControlId()},
		Limit:         req.GetLimit(),
		NodeType:      pb.NodeType_NODE_TYPE_CONTROL,
	}

	searchResp, err := s.graph.SimilaritySearch(ctx, searchReq)
	if err != nil {
		s.recordMetrics(ctx, "FindSimilar", start, status.Code(err))
		return nil, err
	}

	s.recordMetrics(ctx, "FindSimilar", start, codes.OK)

	return &pb.FindSimilarResponse{
		Response: searchResp,
	}, nil
}
