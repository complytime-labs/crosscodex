package graph_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

var _ = Describe("Write RPCs", func() {
	var (
		svc         *graph.Service
		mockGraph   *mockGraphDB
		mockVectors *mockVectorDB
	)

	BeforeEach(func() {
		mockGraph = &mockGraphDB{}
		mockVectors = &mockVectorDB{}
		svc = graph.New(mockGraph, mockVectors, nil)
	})

	Describe("CreateNode", func() {
		It("rejects missing tenant context", func() {
			resp, err := svc.CreateNode(context.Background(), &pb.CreateNodeRequest{})
			Expect(resp).To(BeNil())
			Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
			Expect(err.Error()).To(ContainSubstring("tenant_context is required"))
		})

		It("rejects missing label", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			resp, err := svc.CreateNode(ctx, &pb.CreateNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
			})
			Expect(resp).To(BeNil())
			Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
			Expect(err.Error()).To(ContainSubstring("label is required"))
		})

		It("creates a node with generated ID", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			var capturedNode graphdb.Node

			mockGraph.createNodeFunc = func(ctx context.Context, tenant string, node graphdb.Node) error {
				capturedNode = node
				return nil
			}

			resp, err := svc.CreateNode(ctx, &pb.CreateNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Label:         "Control",
				Properties:    map[string]string{"title": "AC-1"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(resp.NodeId).NotTo(BeEmpty())
			Expect(capturedNode.ID).To(Equal(resp.NodeId))
			Expect(capturedNode.Label).To(Equal("Control"))
			Expect(capturedNode.Properties["title"]).To(Equal("AC-1"))
		})

		It("propagates graphdb errors", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			mockGraph.createNodeFunc = func(ctx context.Context, tenant string, node graphdb.Node) error {
				return graphdb.ErrTenantRequired
			}

			resp, err := svc.CreateNode(ctx, &pb.CreateNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Label:         "Control",
			})
			Expect(resp).To(BeNil())
			Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
		})

		It("preserves temporal attributes", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			now := time.Now().UTC()
			var capturedNode graphdb.Node

			mockGraph.createNodeFunc = func(ctx context.Context, tenant string, node graphdb.Node) error {
				capturedNode = node
				return nil
			}

			resp, err := svc.CreateNode(ctx, &pb.CreateNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Label:         "Control",
				Temporal: &pb.TemporalAttributes{
					ValidFrom: timestamppb.New(now),
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(capturedNode.ValidFrom).To(BeTemporally("~", now, time.Second))
		})
	})

	Describe("CreateEdge", func() {
		It("rejects missing tenant context", func() {
			resp, err := svc.CreateEdge(context.Background(), &pb.CreateEdgeRequest{})
			Expect(resp).To(BeNil())
			Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
		})

		It("rejects missing source_node_id", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			resp, err := svc.CreateEdge(ctx, &pb.CreateEdgeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				TargetNodeId:  "node-2",
				Label:         "maps_to",
			})
			Expect(resp).To(BeNil())
			Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
			Expect(err.Error()).To(ContainSubstring("source_node_id and target_node_id are required"))
		})

		It("rejects missing target_node_id", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			resp, err := svc.CreateEdge(ctx, &pb.CreateEdgeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				SourceNodeId:  "node-1",
				Label:         "maps_to",
			})
			Expect(resp).To(BeNil())
			Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
		})

		It("rejects missing label", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			resp, err := svc.CreateEdge(ctx, &pb.CreateEdgeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				SourceNodeId:  "node-1",
				TargetNodeId:  "node-2",
			})
			Expect(resp).To(BeNil())
			Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
			Expect(err.Error()).To(ContainSubstring("label is required"))
		})

		It("creates an edge with generated ID", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			var capturedSourceID, capturedTargetID string
			var capturedEdge graphdb.Edge

			mockGraph.createEdgeFunc = func(ctx context.Context, tenant, sourceID, targetID string, edge graphdb.Edge) error {
				capturedSourceID = sourceID
				capturedTargetID = targetID
				capturedEdge = edge
				return nil
			}

			resp, err := svc.CreateEdge(ctx, &pb.CreateEdgeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				SourceNodeId:  "node-1",
				TargetNodeId:  "node-2",
				Label:         "maps_to",
				Properties:    map[string]string{"confidence": "0.95"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(resp.EdgeId).NotTo(BeEmpty())
			Expect(capturedEdge.ID).To(Equal(resp.EdgeId))
			Expect(capturedEdge.Label).To(Equal("maps_to"))
			Expect(capturedSourceID).To(Equal("node-1"))
			Expect(capturedTargetID).To(Equal("node-2"))
		})

		It("propagates graphdb errors", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			mockGraph.createEdgeFunc = func(ctx context.Context, tenant, sourceID, targetID string, edge graphdb.Edge) error {
				return graphdb.ErrNodeNotFound
			}

			resp, err := svc.CreateEdge(ctx, &pb.CreateEdgeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				SourceNodeId:  "node-1",
				TargetNodeId:  "node-2",
				Label:         "maps_to",
			})
			Expect(resp).To(BeNil())
			Expect(status.Code(err)).To(Equal(codes.NotFound))
		})
	})

	Describe("BulkCreateEdges", func() {
		It("rejects missing tenant context", func() {
			resp, err := svc.BulkCreateEdges(context.Background(), &pb.BulkCreateEdgesRequest{})
			Expect(resp).To(BeNil())
			Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
		})

		It("handles empty edge list", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			resp, err := svc.BulkCreateEdges(ctx, &pb.BulkCreateEdgesRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Edges:         []*pb.CreateEdgeRequest{},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(resp.CreatedCount).To(Equal(int32(0)))
		})

		It("creates multiple edges in bulk", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			var capturedEdges []graphdb.BulkEdge

			mockGraph.bulkCreateEdgesFunc = func(ctx context.Context, tenant string, edges []graphdb.BulkEdge) ([]string, error) {
				capturedEdges = edges
				ids := make([]string, len(edges))
				for i := range edges {
					ids[i] = edges[i].Edge.ID
				}
				return ids, nil
			}

			resp, err := svc.BulkCreateEdges(ctx, &pb.BulkCreateEdgesRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Edges: []*pb.CreateEdgeRequest{
					{
						SourceNodeId: "node-1",
						TargetNodeId: "node-2",
						Label:        "maps_to",
					},
					{
						SourceNodeId: "node-2",
						TargetNodeId: "node-3",
						Label:        "depends_on",
					},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(resp.CreatedCount).To(Equal(int32(2)))
			Expect(resp.EdgeIds).To(HaveLen(2))
			Expect(capturedEdges).To(HaveLen(2))
			Expect(capturedEdges[0].SourceID).To(Equal("node-1"))
			Expect(capturedEdges[0].TargetID).To(Equal("node-2"))
			Expect(capturedEdges[0].Edge.Label).To(Equal("maps_to"))
			Expect(capturedEdges[1].SourceID).To(Equal("node-2"))
			Expect(capturedEdges[1].TargetID).To(Equal("node-3"))
			Expect(capturedEdges[1].Edge.Label).To(Equal("depends_on"))
		})

		It("returns partial results on error", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			mockGraph.bulkCreateEdgesFunc = func(ctx context.Context, tenant string, edges []graphdb.BulkEdge) ([]string, error) {
				return []string{"edge-1"}, graphdb.ErrNodeNotFound
			}

			resp, err := svc.BulkCreateEdges(ctx, &pb.BulkCreateEdgesRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Edges: []*pb.CreateEdgeRequest{
					{SourceNodeId: "node-1", TargetNodeId: "node-2", Label: "maps_to"},
					{SourceNodeId: "node-3", TargetNodeId: "node-4", Label: "maps_to"},
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(status.Code(err)).To(Equal(codes.NotFound))
			Expect(resp).NotTo(BeNil())
			Expect(resp.CreatedCount).To(Equal(int32(1)))
			Expect(resp.EdgeIds).To(HaveLen(1))
			Expect(resp.Errors).To(HaveLen(1))
			Expect(resp.Errors[0].Code).To(Equal(pb.ErrorCode_ERROR_CODE_INTERNAL))
		})
	})

	Describe("SupersedeFact", func() {
		It("rejects missing tenant context", func() {
			resp, err := svc.SupersedeFact(context.Background(), &pb.SupersedeFactRequest{})
			Expect(resp).To(BeNil())
			Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
		})

		It("rejects missing target", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			resp, err := svc.SupersedeFact(ctx, &pb.SupersedeFactRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
			})
			Expect(resp).To(BeNil())
			Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
			Expect(err.Error()).To(ContainSubstring("node_id or edge_id is required"))
		})

		It("supersedes a node by ID", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			var capturedReq graphdb.SupersedeRequest

			mockGraph.supersedeFactFunc = func(ctx context.Context, tenant string, req graphdb.SupersedeRequest) (bool, error) {
				capturedReq = req
				return true, nil
			}

			resp, err := svc.SupersedeFact(ctx, &pb.SupersedeFactRequest{
				TenantContext:     &pb.TenantContext{TenantId: "test-tenant"},
				Target:            &pb.SupersedeFactRequest_NodeId{NodeId: "node-1"},
				SupersededByJobId: "job-123",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(resp.Updated).To(BeTrue())
			Expect(capturedReq.NodeID).To(Equal("node-1"))
			Expect(capturedReq.EdgeID).To(BeEmpty())
			Expect(capturedReq.SupersededByJobID).To(Equal("job-123"))
		})

		It("supersedes an edge by ID", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			var capturedReq graphdb.SupersedeRequest

			mockGraph.supersedeFactFunc = func(ctx context.Context, tenant string, req graphdb.SupersedeRequest) (bool, error) {
				capturedReq = req
				return true, nil
			}

			resp, err := svc.SupersedeFact(ctx, &pb.SupersedeFactRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Target:        &pb.SupersedeFactRequest_EdgeId{EdgeId: "edge-1"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(resp.Updated).To(BeTrue())
			Expect(capturedReq.EdgeID).To(Equal("edge-1"))
			Expect(capturedReq.NodeID).To(BeEmpty())
		})

		It("uses provided superseded_at timestamp", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			specificTime := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
			var capturedReq graphdb.SupersedeRequest

			mockGraph.supersedeFactFunc = func(ctx context.Context, tenant string, req graphdb.SupersedeRequest) (bool, error) {
				capturedReq = req
				return true, nil
			}

			resp, err := svc.SupersedeFact(ctx, &pb.SupersedeFactRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Target:        &pb.SupersedeFactRequest_NodeId{NodeId: "node-1"},
				SupersededAt:  timestamppb.New(specificTime),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(capturedReq.SupersededAt).To(BeTemporally("~", specificTime, time.Second))
		})

		It("defaults to current time when superseded_at is not provided", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			now := time.Now().UTC()
			var capturedReq graphdb.SupersedeRequest

			mockGraph.supersedeFactFunc = func(ctx context.Context, tenant string, req graphdb.SupersedeRequest) (bool, error) {
				capturedReq = req
				return true, nil
			}

			resp, err := svc.SupersedeFact(ctx, &pb.SupersedeFactRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Target:        &pb.SupersedeFactRequest_NodeId{NodeId: "node-1"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(capturedReq.SupersededAt).To(BeTemporally("~", now, 2*time.Second))
		})

		It("propagates graphdb errors", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			mockGraph.supersedeFactFunc = func(ctx context.Context, tenant string, req graphdb.SupersedeRequest) (bool, error) {
				return false, graphdb.ErrNodeNotFound
			}

			resp, err := svc.SupersedeFact(ctx, &pb.SupersedeFactRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Target:        &pb.SupersedeFactRequest_NodeId{NodeId: "nonexistent"},
			})
			Expect(resp).To(BeNil())
			Expect(status.Code(err)).To(Equal(codes.NotFound))
		})
	})
})
