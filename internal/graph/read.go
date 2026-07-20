package graph

import (
	"context"
	"errors"
	"time"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GetNode retrieves a single node by ID.
func (s *Service) GetNode(ctx context.Context, req *pb.GetNodeRequest) (*pb.GetNodeResponse, error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.GetNode")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "GetNode", start, status.Code(err))
		return nil, err
	}

	if req.GetNodeId() == "" {
		s.recordRPC(ctx, "GetNode", start, codes.InvalidArgument)
		return nil, status.Error(codes.InvalidArgument, "node_id is required")
	}

	node, err := s.graph.GetNode(ctx, tenantID, req.GetNodeId())
	if err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "GetNode", start, code)
		return nil, status.Error(code, err.Error())
	}

	tc := &pb.TenantContext{TenantId: tenantID}
	s.recordRPC(ctx, "GetNode", start, codes.OK)
	return &pb.GetNodeResponse{Node: nodeToProto(*node, tc)}, nil
}

// GetEdge retrieves a single edge by ID, including source and target node IDs.
func (s *Service) GetEdge(ctx context.Context, req *pb.GetEdgeRequest) (*pb.GetEdgeResponse, error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.GetEdge")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "GetEdge", start, status.Code(err))
		return nil, err
	}

	if req.GetEdgeId() == "" {
		s.recordRPC(ctx, "GetEdge", start, codes.InvalidArgument)
		return nil, status.Error(codes.InvalidArgument, "edge_id is required")
	}

	edge, err := s.graph.GetEdge(ctx, tenantID, req.GetEdgeId())
	if err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "GetEdge", start, code)
		return nil, status.Error(code, err.Error())
	}

	tc := &pb.TenantContext{TenantId: tenantID}
	s.recordRPC(ctx, "GetEdge", start, codes.OK)
	return &pb.GetEdgeResponse{Edge: edgeToProto(*edge, tc)}, nil
}

// Traverse performs graph traversal from a starting node.
func (s *Service) Traverse(ctx context.Context, req *pb.TraverseRequest) (*pb.TraverseResponse, error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.Traverse")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "Traverse", start, status.Code(err))
		return nil, err
	}

	if req.GetStartNodeId() == "" {
		s.recordRPC(ctx, "Traverse", start, codes.InvalidArgument)
		return nil, status.Error(codes.InvalidArgument, "start_node_id is required")
	}

	query := graphdb.TraversalQuery{
		StartNode:  req.GetStartNodeId(),
		Direction:  protoDirectionToString(req.GetDirection()),
		EdgeLabels: req.GetEdgeLabels(),
		MaxDepth:   int(req.GetMaxDepth()),
	}
	if req.GetAsOf() != nil {
		t := req.GetAsOf().AsTime()
		query.AsOf = &t
	}

	paths, err := s.graph.Traverse(ctx, tenantID, query)
	if err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "Traverse", start, code)
		return nil, status.Error(code, err.Error())
	}

	tc := &pb.TenantContext{TenantId: tenantID}
	s.recordRPC(ctx, "Traverse", start, codes.OK)
	return pathToTraverseResponse(paths, tc), nil
}

// Query executes a read-only openCypher query. Admin-only.
func (s *Service) Query(ctx context.Context, req *pb.QueryRequest) (*pb.QueryResponse, error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.Query")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "Query", start, status.Code(err))
		return nil, err
	}

	if err := s.requireAdmin(ctx); err != nil {
		s.recordRPC(ctx, "Query", start, codes.PermissionDenied)
		return nil, err
	}

	if req.GetCypher() == "" {
		s.recordRPC(ctx, "Query", start, codes.InvalidArgument)
		return nil, status.Error(codes.InvalidArgument, "cypher is required")
	}

	rows, err := s.graph.ExecuteQuery(ctx, tenantID, req.GetCypher(), req.GetParameters())
	if err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "Query", start, code)
		return nil, status.Error(code, err.Error())
	}

	s.recordRPC(ctx, "Query", start, codes.OK)
	return queryRowsToProto(rows), nil
}

// SimilaritySearch performs vector similarity search.
func (s *Service) SimilaritySearch(ctx context.Context, req *pb.SimilaritySearchRequest) (*pb.SimilaritySearchResponse, error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.SimilaritySearch")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "SimilaritySearch", start, status.Code(err))
		return nil, err
	}

	if s.vectors == nil {
		s.recordRPC(ctx, "SimilaritySearch", start, codes.FailedPrecondition)
		return nil, status.Error(codes.FailedPrecondition, "vector search not configured")
	}

	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 10
	}

	var results []vectordb.SimilarityResult
	switch q := req.GetQuery().(type) {
	case *pb.SimilaritySearchRequest_QueryEmbedding:
		if q.QueryEmbedding == nil || len(q.QueryEmbedding.GetEmbeddings()) == 0 {
			s.recordRPC(ctx, "SimilaritySearch", start, codes.InvalidArgument)
			return nil, status.Error(codes.InvalidArgument, "query_embedding is required")
		}
		vector := q.QueryEmbedding.GetEmbeddings()
		// For query_embedding, search using "generic" catalog/model defaults.
		// Vectordb requires exact catalog_id and model matches; cross-catalog
		// search is not supported in the current vectordb implementation.
		results, err = s.vectors.FindSimilar(ctx, tenantID, vectordb.FindSimilarQuery{
			CatalogID: "generic",
			Model:     "generic",
			Vector:    vector,
			Limit:     limit,
		})
	case *pb.SimilaritySearchRequest_ControlId:
		if q.ControlId == "" {
			s.recordRPC(ctx, "SimilaritySearch", start, codes.InvalidArgument)
			return nil, status.Error(codes.InvalidArgument, "control_id is required")
		}
		// For control_id search, use the control ID as catalog_id with generic model.
		results, err = s.vectors.FindSimilar(ctx, tenantID, vectordb.FindSimilarQuery{
			CatalogID: q.ControlId,
			Model:     "generic",
			Limit:     limit,
		})
	default:
		s.recordRPC(ctx, "SimilaritySearch", start, codes.InvalidArgument)
		return nil, status.Error(codes.InvalidArgument, "query_embedding or control_id is required")
	}
	if err != nil {
		code := mapVectorError(err)
		s.recordRPC(ctx, "SimilaritySearch", start, code)
		return nil, status.Error(code, err.Error())
	}

	tc := &pb.TenantContext{TenantId: tenantID}
	resp := &pb.SimilaritySearchResponse{}
	for _, r := range results {
		resp.Matches = append(resp.Matches, similarityResultToProto(r, tc))
	}
	s.recordRPC(ctx, "SimilaritySearch", start, codes.OK)
	return resp, nil
}

// TemporalQuery executes a temporal point-in-time query. Admin-only.
func (s *Service) TemporalQuery(ctx context.Context, req *pb.TemporalQueryRequest) (*pb.QueryResponse, error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.TemporalQuery")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "TemporalQuery", start, status.Code(err))
		return nil, err
	}

	if err := s.requireAdmin(ctx); err != nil {
		s.recordRPC(ctx, "TemporalQuery", start, codes.PermissionDenied)
		return nil, err
	}

	if req.GetCypher() == "" {
		s.recordRPC(ctx, "TemporalQuery", start, codes.InvalidArgument)
		return nil, status.Error(codes.InvalidArgument, "cypher is required")
	}
	if req.GetAsOf() == nil {
		s.recordRPC(ctx, "TemporalQuery", start, codes.InvalidArgument)
		return nil, status.Error(codes.InvalidArgument, "as_of is required")
	}

	// Inject as_of timestamp into parameters so the caller's Cypher can reference it via $as_of.
	// The caller is responsible for writing temporal filtering logic in their query.
	params := req.GetParameters()
	if params == nil {
		params = make(map[string]string)
	}
	params["as_of"] = req.GetAsOf().AsTime().Format(time.RFC3339Nano)

	rows, err := s.graph.ExecuteQuery(ctx, tenantID, req.GetCypher(), params)
	if err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "TemporalQuery", start, code)
		return nil, status.Error(code, err.Error())
	}

	s.recordRPC(ctx, "TemporalQuery", start, codes.OK)
	return queryRowsToProto(rows), nil
}

// requireAdmin checks the identity in context for admin role.
func (s *Service) requireAdmin(ctx context.Context) error {
	identity := identityFromContext(ctx)
	if identity == nil {
		return status.Error(codes.Unauthenticated, "not authenticated")
	}
	if err := authn.RequireRole(*identity, authn.RoleAdmin); err != nil {
		return status.Errorf(codes.PermissionDenied, "admin access required: %v", err)
	}
	return nil
}

// identityFromContext extracts the authn.Identity from context.
// Uses the shared context helper from pkg/authn.
func identityFromContext(ctx context.Context) *authn.Identity {
	return authn.IdentityFromContext(ctx)
}

// protoDirectionToString converts proto TraversalDirection to string.
func protoDirectionToString(d pb.TraversalDirection) string {
	switch d {
	case pb.TraversalDirection_TRAVERSAL_DIRECTION_INBOUND:
		return "inbound"
	case pb.TraversalDirection_TRAVERSAL_DIRECTION_BOTH:
		return "both"
	default:
		return "outbound"
	}
}

// mapGraphError maps graphdb errors to gRPC status codes.
func mapGraphError(err error) codes.Code {
	switch {
	case err == nil:
		return codes.OK
	case errors.Is(err, graphdb.ErrNodeNotFound),
		errors.Is(err, graphdb.ErrEdgeNotFound),
		errors.Is(err, graphdb.ErrGraphNotFound):
		return codes.NotFound
	case errors.Is(err, graphdb.ErrInvalidCypher),
		errors.Is(err, graphdb.ErrTenantRequired):
		return codes.InvalidArgument
	case errors.Is(err, graphdb.ErrReadOnlyViolation):
		return codes.PermissionDenied
	default:
		return codes.Internal
	}
}

// mapVectorError maps vectordb errors to gRPC status codes.
func mapVectorError(err error) codes.Code {
	switch {
	case err == nil:
		return codes.OK
	case errors.Is(err, vectordb.ErrNotFound),
		errors.Is(err, vectordb.ErrModelNotFound):
		return codes.NotFound
	case errors.Is(err, vectordb.ErrInvalidDimension):
		return codes.InvalidArgument
	default:
		return codes.Internal
	}
}
