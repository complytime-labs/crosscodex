package graph

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
)

// GetNode retrieves a single node by ID.
func (s *Service) GetNode(ctx context.Context, req *connect.Request[pb.GetNodeRequest]) (*connect.Response[pb.GetNodeResponse], error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.GetNode")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.Msg.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "GetNode", start, connect.CodeOf(err))
		return nil, err
	}

	if req.Msg.GetNodeId() == "" {
		s.recordRPC(ctx, "GetNode", start, connect.CodeInvalidArgument)
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("node_id is required"))
	}

	node, err := s.graph.GetNode(ctx, tenantID, req.Msg.GetNodeId())
	if err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "GetNode", start, code)
		return nil, connect.NewError(code, errors.New(err.Error()))
	}

	tc := &pb.TenantContext{TenantId: tenantID}
	s.recordRPC(ctx, "GetNode", start, connect.Code(0))
	return connect.NewResponse(&pb.GetNodeResponse{Node: nodeToProto(*node, tc)}), nil
}

// GetEdge retrieves a single edge by ID, including source and target node IDs.
func (s *Service) GetEdge(ctx context.Context, req *connect.Request[pb.GetEdgeRequest]) (*connect.Response[pb.GetEdgeResponse], error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.GetEdge")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.Msg.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "GetEdge", start, connect.CodeOf(err))
		return nil, err
	}

	if req.Msg.GetEdgeId() == "" {
		s.recordRPC(ctx, "GetEdge", start, connect.CodeInvalidArgument)
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("edge_id is required"))
	}

	edge, err := s.graph.GetEdge(ctx, tenantID, req.Msg.GetEdgeId())
	if err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "GetEdge", start, code)
		return nil, connect.NewError(code, errors.New(err.Error()))
	}

	tc := &pb.TenantContext{TenantId: tenantID}
	s.recordRPC(ctx, "GetEdge", start, connect.Code(0))
	return connect.NewResponse(&pb.GetEdgeResponse{Edge: edgeToProto(*edge, tc)}), nil
}

// Traverse performs graph traversal from a starting node.
func (s *Service) Traverse(ctx context.Context, req *connect.Request[pb.TraverseRequest]) (*connect.Response[pb.TraverseResponse], error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.Traverse")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.Msg.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "Traverse", start, connect.CodeOf(err))
		return nil, err
	}

	if req.Msg.GetStartNodeId() == "" {
		s.recordRPC(ctx, "Traverse", start, connect.CodeInvalidArgument)
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("start_node_id is required"))
	}

	query := graphdb.TraversalQuery{
		StartNode:  req.Msg.GetStartNodeId(),
		Direction:  protoDirectionToString(req.Msg.GetDirection()),
		EdgeLabels: req.Msg.GetEdgeLabels(),
		MaxDepth:   int(req.Msg.GetMaxDepth()),
	}
	if req.Msg.GetAsOf() != nil {
		t := req.Msg.GetAsOf().AsTime()
		query.AsOf = &t
	}

	paths, err := s.graph.Traverse(ctx, tenantID, query)
	if err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "Traverse", start, code)
		return nil, connect.NewError(code, errors.New(err.Error()))
	}

	tc := &pb.TenantContext{TenantId: tenantID}
	s.recordRPC(ctx, "Traverse", start, connect.Code(0))
	return connect.NewResponse(pathToTraverseResponse(paths, tc)), nil
}

// Query executes a read-only openCypher query. Admin-only.
func (s *Service) Query(ctx context.Context, req *connect.Request[pb.QueryRequest]) (*connect.Response[pb.QueryResponse], error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.Query")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.Msg.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "Query", start, connect.CodeOf(err))
		return nil, err
	}

	if err := s.requireAdmin(ctx); err != nil {
		s.recordRPC(ctx, "Query", start, connect.CodePermissionDenied)
		return nil, err
	}

	if req.Msg.GetCypher() == "" {
		s.recordRPC(ctx, "Query", start, connect.CodeInvalidArgument)
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cypher is required"))
	}

	rows, err := s.graph.ExecuteQuery(ctx, tenantID, req.Msg.GetCypher(), req.Msg.GetParameters())
	if err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "Query", start, code)
		return nil, connect.NewError(code, errors.New(err.Error()))
	}

	s.recordRPC(ctx, "Query", start, connect.Code(0))
	return connect.NewResponse(queryRowsToProto(rows)), nil
}

// SimilaritySearch performs vector similarity search.
func (s *Service) SimilaritySearch(ctx context.Context, req *connect.Request[pb.SimilaritySearchRequest]) (*connect.Response[pb.SimilaritySearchResponse], error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.SimilaritySearch")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.Msg.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "SimilaritySearch", start, connect.CodeOf(err))
		return nil, err
	}

	if s.vectors == nil {
		s.recordRPC(ctx, "SimilaritySearch", start, connect.CodeFailedPrecondition)
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("vector search not configured"))
	}

	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = 10
	}

	var results []vectordb.SimilarityResult
	switch q := req.Msg.GetQuery().(type) {
	case *pb.SimilaritySearchRequest_QueryEmbedding:
		if q.QueryEmbedding == nil || len(q.QueryEmbedding.GetEmbeddings()) == 0 {
			s.recordRPC(ctx, "SimilaritySearch", start, connect.CodeInvalidArgument)
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("query_embedding is required"))
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
			s.recordRPC(ctx, "SimilaritySearch", start, connect.CodeInvalidArgument)
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("control_id is required"))
		}
		// For control_id search, use the control ID as catalog_id with generic model.
		results, err = s.vectors.FindSimilar(ctx, tenantID, vectordb.FindSimilarQuery{
			CatalogID: q.ControlId,
			Model:     "generic",
			Limit:     limit,
		})
	default:
		s.recordRPC(ctx, "SimilaritySearch", start, connect.CodeInvalidArgument)
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("query_embedding or control_id is required"))
	}
	if err != nil {
		code := mapVectorError(err)
		s.recordRPC(ctx, "SimilaritySearch", start, code)
		return nil, connect.NewError(code, errors.New(err.Error()))
	}

	tc := &pb.TenantContext{TenantId: tenantID}
	resp := &pb.SimilaritySearchResponse{}
	for _, r := range results {
		resp.Matches = append(resp.Matches, similarityResultToProto(r, tc))
	}
	s.recordRPC(ctx, "SimilaritySearch", start, connect.Code(0))
	return connect.NewResponse(resp), nil
}

// TemporalQuery executes a temporal point-in-time query. Admin-only.
func (s *Service) TemporalQuery(ctx context.Context, req *connect.Request[pb.TemporalQueryRequest]) (*connect.Response[pb.QueryResponse], error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.TemporalQuery")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.Msg.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "TemporalQuery", start, connect.CodeOf(err))
		return nil, err
	}

	if err := s.requireAdmin(ctx); err != nil {
		s.recordRPC(ctx, "TemporalQuery", start, connect.CodePermissionDenied)
		return nil, err
	}

	if req.Msg.GetCypher() == "" {
		s.recordRPC(ctx, "TemporalQuery", start, connect.CodeInvalidArgument)
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cypher is required"))
	}
	if req.Msg.GetAsOf() == nil {
		s.recordRPC(ctx, "TemporalQuery", start, connect.CodeInvalidArgument)
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("as_of is required"))
	}

	// Inject as_of timestamp into parameters so the caller's Cypher can reference it via $as_of.
	// The caller is responsible for writing temporal filtering logic in their query.
	params := req.Msg.GetParameters()
	if params == nil {
		params = make(map[string]string)
	}
	params["as_of"] = req.Msg.GetAsOf().AsTime().Format(time.RFC3339Nano)

	rows, err := s.graph.ExecuteQuery(ctx, tenantID, req.Msg.GetCypher(), params)
	if err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "TemporalQuery", start, code)
		return nil, connect.NewError(code, errors.New(err.Error()))
	}

	s.recordRPC(ctx, "TemporalQuery", start, connect.Code(0))
	return connect.NewResponse(queryRowsToProto(rows)), nil
}

// requireAdmin checks the identity in context for admin role.
func (s *Service) requireAdmin(ctx context.Context) error {
	identity := identityFromContext(ctx)
	if identity == nil {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}
	if err := authn.RequireRole(*identity, authn.RoleAdmin); err != nil {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("admin access required: %v", err))
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

// mapGraphError maps graphdb errors to Connect status codes.
func mapGraphError(err error) connect.Code {
	switch {
	case err == nil:
		return connect.Code(0)
	case errors.Is(err, graphdb.ErrNodeNotFound),
		errors.Is(err, graphdb.ErrEdgeNotFound),
		errors.Is(err, graphdb.ErrGraphNotFound):
		return connect.CodeNotFound
	case errors.Is(err, graphdb.ErrInvalidCypher),
		errors.Is(err, graphdb.ErrTenantRequired):
		return connect.CodeInvalidArgument
	case errors.Is(err, graphdb.ErrReadOnlyViolation):
		return connect.CodePermissionDenied
	default:
		return connect.CodeInternal
	}
}

// mapVectorError maps vectordb errors to Connect status codes.
func mapVectorError(err error) connect.Code {
	switch {
	case err == nil:
		return connect.Code(0)
	case errors.Is(err, vectordb.ErrNotFound),
		errors.Is(err, vectordb.ErrModelNotFound):
		return connect.CodeNotFound
	case errors.Is(err, vectordb.ErrInvalidDimension):
		return connect.CodeInvalidArgument
	default:
		return connect.CodeInternal
	}
}
