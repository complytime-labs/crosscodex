package graph

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CreateNode creates a new node in the graph.
func (s *Service) CreateNode(ctx context.Context, req *pb.CreateNodeRequest) (*pb.CreateNodeResponse, error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.CreateNode")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "CreateNode", start, status.Code(err))
		return nil, err
	}

	if req.GetLabel() == "" {
		s.recordRPC(ctx, "CreateNode", start, codes.InvalidArgument)
		return nil, status.Error(codes.InvalidArgument, "label is required")
	}

	node := protoToNode(req)
	node.ID = generateID()

	if err := s.graph.CreateNode(ctx, tenantID, node); err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "CreateNode", start, code)
		return nil, status.Error(code, err.Error())
	}

	s.recordRPC(ctx, "CreateNode", start, codes.OK)
	return &pb.CreateNodeResponse{NodeId: node.ID}, nil
}

// CreateEdge creates a new edge between two nodes.
func (s *Service) CreateEdge(ctx context.Context, req *pb.CreateEdgeRequest) (*pb.CreateEdgeResponse, error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.CreateEdge")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "CreateEdge", start, status.Code(err))
		return nil, err
	}

	if req.GetSourceNodeId() == "" || req.GetTargetNodeId() == "" {
		s.recordRPC(ctx, "CreateEdge", start, codes.InvalidArgument)
		return nil, status.Error(codes.InvalidArgument, "source_node_id and target_node_id are required")
	}
	if req.GetLabel() == "" {
		s.recordRPC(ctx, "CreateEdge", start, codes.InvalidArgument)
		return nil, status.Error(codes.InvalidArgument, "label is required")
	}

	edge := protoToEdge(req)
	edge.ID = generateID()

	if err := s.graph.CreateEdge(ctx, tenantID, req.GetSourceNodeId(), req.GetTargetNodeId(), edge); err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "CreateEdge", start, code)
		return nil, status.Error(code, err.Error())
	}

	s.recordRPC(ctx, "CreateEdge", start, codes.OK)
	return &pb.CreateEdgeResponse{EdgeId: edge.ID}, nil
}

// BulkCreateEdges creates multiple edges in a single transaction.
func (s *Service) BulkCreateEdges(ctx context.Context, req *pb.BulkCreateEdgesRequest) (*pb.BulkCreateEdgesResponse, error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.BulkCreateEdges")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "BulkCreateEdges", start, status.Code(err))
		return nil, err
	}

	if len(req.GetEdges()) == 0 {
		s.recordRPC(ctx, "BulkCreateEdges", start, codes.OK)
		return &pb.BulkCreateEdgesResponse{}, nil
	}

	bulkEdges := make([]graphdb.BulkEdge, len(req.GetEdges()))
	for i, pe := range req.GetEdges() {
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
		return resp, status.Error(code, err.Error())
	}

	s.recordRPC(ctx, "BulkCreateEdges", start, codes.OK)
	return &pb.BulkCreateEdgesResponse{
		EdgeIds:      ids,
		CreatedCount: int32(len(ids)),
	}, nil
}

// SupersedeFact marks a node or edge as temporally superseded.
func (s *Service) SupersedeFact(ctx context.Context, req *pb.SupersedeFactRequest) (*pb.SupersedeFactResponse, error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.SupersedeFact")
	defer span.End()

	tenantID, err := s.extractTenant(ctx, req.GetTenantContext())
	if err != nil {
		s.recordRPC(ctx, "SupersedeFact", start, status.Code(err))
		return nil, err
	}

	supersededAt := time.Now().UTC()
	if req.GetSupersededAt() != nil {
		supersededAt = req.GetSupersededAt().AsTime()
	}

	gReq := graphdb.SupersedeRequest{
		SupersededAt:      supersededAt,
		SupersededByJobID: req.GetSupersededByJobId(),
	}

	switch t := req.GetTarget().(type) {
	case *pb.SupersedeFactRequest_NodeId:
		gReq.NodeID = t.NodeId
	case *pb.SupersedeFactRequest_EdgeId:
		gReq.EdgeID = t.EdgeId
	default:
		s.recordRPC(ctx, "SupersedeFact", start, codes.InvalidArgument)
		return nil, status.Error(codes.InvalidArgument, "node_id or edge_id is required")
	}

	updated, err := s.graph.SupersedeFact(ctx, tenantID, gReq)
	if err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "SupersedeFact", start, code)
		return nil, status.Error(code, err.Error())
	}

	s.recordRPC(ctx, "SupersedeFact", start, codes.OK)
	return &pb.SupersedeFactResponse{Updated: updated}, nil
}

// generateID produces a random hex ID.
func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}
