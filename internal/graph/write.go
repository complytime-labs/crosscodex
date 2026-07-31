package graph

import (
	"connectrpc.com/connect"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

// CreateNode creates a new node in the graph.
func (s *Service) CreateNode(ctx context.Context, req *connect.Request[pb.CreateNodeRequest]) (*connect.Response[pb.CreateNodeResponse], error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.CreateNode")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.Msg.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "CreateNode", start, connect.CodeOf(err))
		return nil, err
	}

	if req.Msg.GetLabel() == "" {
		s.recordRPC(ctx, "CreateNode", start, connect.CodeInvalidArgument)
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("label is required"))
	}

	node := protoToNode(req.Msg)
	node.ID = generateID()

	if err := s.graph.CreateNode(ctx, tenantID, node); err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "CreateNode", start, code)
		return nil, connect.NewError(code, errors.New(err.Error()))
	}

	s.recordRPC(ctx, "CreateNode", start, connect.Code(0))
	return connect.NewResponse(&pb.CreateNodeResponse{NodeId: node.ID}), nil
}

// CreateEdge creates a new edge between two nodes.
func (s *Service) CreateEdge(ctx context.Context, req *connect.Request[pb.CreateEdgeRequest]) (*connect.Response[pb.CreateEdgeResponse], error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.CreateEdge")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.Msg.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "CreateEdge", start, connect.CodeOf(err))
		return nil, err
	}

	if req.Msg.GetSourceNodeId() == "" || req.Msg.GetTargetNodeId() == "" {
		s.recordRPC(ctx, "CreateEdge", start, connect.CodeInvalidArgument)
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("source_node_id and target_node_id are required"))
	}
	if req.Msg.GetLabel() == "" {
		s.recordRPC(ctx, "CreateEdge", start, connect.CodeInvalidArgument)
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("label is required"))
	}

	edge := protoToEdge(req.Msg)
	edge.ID = generateID()

	if err := s.graph.CreateEdge(ctx, tenantID, req.Msg.GetSourceNodeId(), req.Msg.GetTargetNodeId(), edge); err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "CreateEdge", start, code)
		return nil, connect.NewError(code, errors.New(err.Error()))
	}

	s.recordRPC(ctx, "CreateEdge", start, connect.Code(0))
	return connect.NewResponse(&pb.CreateEdgeResponse{EdgeId: edge.ID}), nil
}

// BulkCreateEdges creates multiple edges in a single transaction.
func (s *Service) BulkCreateEdges(ctx context.Context, req *connect.Request[pb.BulkCreateEdgesRequest]) (*connect.Response[pb.BulkCreateEdgesResponse], error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.BulkCreateEdges")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.Msg.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "BulkCreateEdges", start, connect.CodeOf(err))
		return nil, err
	}

	if len(req.Msg.GetEdges()) == 0 {
		s.recordRPC(ctx, "BulkCreateEdges", start, connect.Code(0))
		return connect.NewResponse(&pb.BulkCreateEdgesResponse{}), nil
	}

	bulkEdges := make([]graphdb.BulkEdge, len(req.Msg.GetEdges()))
	for i, pe := range req.Msg.GetEdges() {
		edge := protoToEdge(pe)
		edge.ID = generateID()
		bulkEdges[i] = graphdb.BulkEdge{
			SourceID: pe.GetSourceNodeId(),
			TargetID: pe.GetTargetNodeId(),
			Edge:     edge,
		}
	}

	ids, err := s.graph.BulkCreateEdges(ctx, tenantID, bulkEdges)
	if err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "BulkCreateEdges", start, code)
		// Use ERROR_CODE_INTERNAL for all bulk errors. BulkCreateEdges is
		// transactional, so partial failures are edge cases where we don't
		// want to expose fine-grained error types in the Error struct (the
		// gRPC status code carries the precise error type).
		resp := &pb.BulkCreateEdgesResponse{
			EdgeIds:      ids,
			CreatedCount: int32(len(ids)),
			Errors: []*pb.Error{{
				Code:    pb.ErrorCode_ERROR_CODE_INTERNAL,
				Message: err.Error(),
			}},
		}
		return connect.NewResponse(resp), connect.NewError(code, errors.New(err.Error()))
	}

	s.recordRPC(ctx, "BulkCreateEdges", start, connect.Code(0))
	return connect.NewResponse(&pb.BulkCreateEdgesResponse{
		EdgeIds:      ids,
		CreatedCount: int32(len(ids)),
	}), nil
}

// SupersedeFact marks a node or edge as temporally superseded.
func (s *Service) SupersedeFact(ctx context.Context, req *connect.Request[pb.SupersedeFactRequest]) (*connect.Response[pb.SupersedeFactResponse], error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.SupersedeFact")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.Msg.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "SupersedeFact", start, connect.CodeOf(err))
		return nil, err
	}

	supersededAt := time.Now().UTC()
	if req.Msg.GetSupersededAt() != nil {
		supersededAt = req.Msg.GetSupersededAt().AsTime()
	}

	gReq := graphdb.SupersedeRequest{
		SupersededAt:      supersededAt,
		SupersededByJobID: req.Msg.GetSupersededByJobId(),
	}

	switch t := req.Msg.GetTarget().(type) {
	case *pb.SupersedeFactRequest_NodeId:
		gReq.NodeID = t.NodeId
	case *pb.SupersedeFactRequest_EdgeId:
		gReq.EdgeID = t.EdgeId
	default:
		s.recordRPC(ctx, "SupersedeFact", start, connect.CodeInvalidArgument)
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("node_id or edge_id is required"))
	}

	updated, err := s.graph.SupersedeFact(ctx, tenantID, gReq)
	if err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "SupersedeFact", start, code)
		return nil, connect.NewError(code, errors.New(err.Error()))
	}

	s.recordRPC(ctx, "SupersedeFact", start, connect.Code(0))
	return connect.NewResponse(&pb.SupersedeFactResponse{Updated: updated}), nil
}

// generateID produces a random hex ID.
func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}
