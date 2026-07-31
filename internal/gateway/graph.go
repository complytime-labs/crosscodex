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

	if s.graph == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("graph backend not configured"))
	}

	tc := buildTenantContext(identity)

	// Translate to TraverseRequest
	traverseReq := &pb.TraverseRequest{
		TenantContext: tc,
		StartNodeId:   req.Msg.GetControlId(),
		Direction:     pb.TraversalDirection_TRAVERSAL_DIRECTION_OUTBOUND,
		EdgeLabels:    []string{"maps_to"},
		MaxDepth:      1,
	}

	if req.Msg.GetLimit() > 0 {
		traverseReq.Options = &pb.ListOptions{
			Pagination: &pb.Pagination{
				PageSize: req.Msg.GetLimit(),
			},
		}
	}

	traverseResp, err := s.graph.Traverse(ctx, connect.NewRequest(traverseReq))
	if err != nil {
		s.recordMetrics(ctx, "GetControlMappings", start, connect.CodeOf(err))
		return nil, err
	}

	// Convert edges to ControlMappings
	mappings := make([]*pb.ControlMapping, 0, len(traverseResp.Msg.GetEdges()))
	for _, edge := range traverseResp.Msg.GetEdges() {
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
		if req.Msg.GetMinConfidence() > 0 && mapping.Confidence < req.Msg.GetMinConfidence() {
			continue
		}

		mappings = append(mappings, mapping)
	}

	s.recordMetrics(ctx, "GetControlMappings", start, connect.Code(0))

	return connect.NewResponse(&pb.GetControlMappingsResponse{
		Mappings: mappings,
		PageInfo: traverseResp.Msg.GetPageInfo(),
	}), nil
}

func (s *Service) QueryGraph(ctx context.Context, req *connect.Request[pb.QueryGraphRequest]) (*connect.Response[pb.QueryGraphResponse], error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}

	// Admin-only endpoint
	if err := authn.RequireRole(*identity, authn.RoleAdmin); err != nil {
		s.recordMetrics(ctx, "QueryGraph", start, connect.CodePermissionDenied)
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("admin access required"))
	}

	ctx, endSpan := s.startHandlerSpan(ctx, "QueryGraph", identity)
	defer endSpan()

	if req.Msg.GetCypher() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cypher is required"))
	}

	if s.graph == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("graph backend not configured"))
	}

	tc := buildTenantContext(identity)

	// Delegate to graph backend's Query
	queryReq := &pb.QueryRequest{
		TenantContext: tc,
		Cypher:        req.Msg.GetCypher(),
		Parameters:    req.Msg.GetParameters(),
	}

	queryResp, err := s.graph.Query(ctx, connect.NewRequest(queryReq))
	if err != nil {
		s.recordMetrics(ctx, "QueryGraph", start, connect.CodeOf(err))
		return nil, err
	}

	s.recordMetrics(ctx, "QueryGraph", start, connect.Code(0))

	return connect.NewResponse(&pb.QueryGraphResponse{
		Response: queryResp.Msg,
	}), nil
}

func (s *Service) FindSimilar(ctx context.Context, req *connect.Request[pb.FindSimilarRequest]) (*connect.Response[pb.FindSimilarResponse], error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}

	ctx, endSpan := s.startHandlerSpan(ctx, "FindSimilar", identity,
		attribute.String("control.id", req.Msg.GetControlId()))
	defer endSpan()

	if req.Msg.GetControlId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("control_id is required"))
	}

	if s.graph == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("graph backend not configured"))
	}

	tc := buildTenantContext(identity)

	// Translate to SimilaritySearchRequest
	searchReq := &pb.SimilaritySearchRequest{
		TenantContext: tc,
		Query:         &pb.SimilaritySearchRequest_ControlId{ControlId: req.Msg.GetControlId()},
		Limit:         req.Msg.GetLimit(),
		NodeType:      pb.NodeType_NODE_TYPE_CONTROL,
	}

	searchResp, err := s.graph.SimilaritySearch(ctx, connect.NewRequest(searchReq))
	if err != nil {
		s.recordMetrics(ctx, "FindSimilar", start, connect.CodeOf(err))
		return nil, err
	}

	s.recordMetrics(ctx, "FindSimilar", start, connect.Code(0))

	return connect.NewResponse(&pb.FindSimilarResponse{
		Response: searchResp.Msg,
	}), nil
}
