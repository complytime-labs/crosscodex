package graph_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
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
			resp, err := svc.CreateNode(context.Background(), connect.NewRequest(&pb.CreateNodeRequest{}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("tenant_context is required"))
		})

		It("rejects missing label", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			resp, err := svc.CreateNode(ctx, connect.NewRequest(&pb.CreateNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("label is required"))
		})

		It("creates a node with generated ID", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			var capturedNode graphdb.Node

			mockGraph.createNodeFunc = func(ctx context.Context, tenant string, node graphdb.Node) error {
				capturedNode = node
				return nil
			}

			resp, err := svc.CreateNode(ctx, connect.NewRequest(&pb.CreateNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Label:         "Control",
				Properties:    map[string]string{"title": "AC-1"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(resp.Msg.NodeId).NotTo(BeEmpty())
			Expect(capturedNode.ID).To(Equal(resp.Msg.NodeId))
			Expect(capturedNode.Label).To(Equal("Control"))
			Expect(capturedNode.Properties["title"]).To(Equal("AC-1"))
		})

		It("propagates graphdb errors", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			mockGraph.createNodeFunc = func(ctx context.Context, tenant string, node graphdb.Node) error {
				return graphdb.ErrTenantRequired
			}

			resp, err := svc.CreateNode(ctx, connect.NewRequest(&pb.CreateNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Label:         "Control",
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("preserves temporal attributes", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			now := time.Now().UTC()
			var capturedNode graphdb.Node

			mockGraph.createNodeFunc = func(ctx context.Context, tenant string, node graphdb.Node) error {
				capturedNode = node
				return nil
			}

			resp, err := svc.CreateNode(ctx, connect.NewRequest(&pb.CreateNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Label:         "Control",
				Temporal: &pb.TemporalAttributes{
					ValidFrom: timestamppb.New(now),
				},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(capturedNode.ValidFrom).To(BeTemporally("~", now, time.Second))
		})
	})

	Describe("CreateEdge", func() {
		It("rejects missing tenant context", func() {
			resp, err := svc.CreateEdge(context.Background(), connect.NewRequest(&pb.CreateEdgeRequest{}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("rejects missing source_node_id", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			resp, err := svc.CreateEdge(ctx, connect.NewRequest(&pb.CreateEdgeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				TargetNodeId:  "node-2",
				Label:         "maps_to",
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("source_node_id and target_node_id are required"))
		})

		It("rejects missing target_node_id", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			resp, err := svc.CreateEdge(ctx, connect.NewRequest(&pb.CreateEdgeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				SourceNodeId:  "node-1",
				Label:         "maps_to",
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("rejects missing label", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			resp, err := svc.CreateEdge(ctx, connect.NewRequest(&pb.CreateEdgeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				SourceNodeId:  "node-1",
				TargetNodeId:  "node-2",
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
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

			resp, err := svc.CreateEdge(ctx, connect.NewRequest(&pb.CreateEdgeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				SourceNodeId:  "node-1",
				TargetNodeId:  "node-2",
				Label:         "maps_to",
				Temporal:      &pb.TemporalAttributes{Confidence: 0.95, DeterminedBy: "job-7"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(resp.Msg.EdgeId).NotTo(BeEmpty())
			Expect(capturedEdge.ID).To(Equal(resp.Msg.EdgeId))
			Expect(capturedEdge.Label).To(Equal("maps_to"))
			Expect(capturedEdge.Confidence).To(Equal(0.95))
			Expect(capturedEdge.DeterminedBy).To(Equal("job-7"))
			Expect(capturedEdge.Properties).NotTo(HaveKey("confidence"))
			Expect(capturedSourceID).To(Equal("node-1"))
			Expect(capturedTargetID).To(Equal("node-2"))
		})

		It("propagates graphdb errors", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			mockGraph.createEdgeFunc = func(ctx context.Context, tenant, sourceID, targetID string, edge graphdb.Edge) error {
				return graphdb.ErrNodeNotFound
			}

			resp, err := svc.CreateEdge(ctx, connect.NewRequest(&pb.CreateEdgeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				SourceNodeId:  "node-1",
				TargetNodeId:  "node-2",
				Label:         "maps_to",
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
		})
	})

	Describe("BulkCreateEdges", func() {
		It("rejects missing tenant context", func() {
			resp, err := svc.BulkCreateEdges(context.Background(), connect.NewRequest(&pb.BulkCreateEdgesRequest{}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("handles empty edge list", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			resp, err := svc.BulkCreateEdges(ctx, connect.NewRequest(&pb.BulkCreateEdgesRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Edges:         []*pb.CreateEdgeRequest{},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(resp.Msg.CreatedCount).To(Equal(int32(0)))
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

			resp, err := svc.BulkCreateEdges(ctx, connect.NewRequest(&pb.BulkCreateEdgesRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Edges: []*pb.CreateEdgeRequest{
					{
						SourceNodeId: "node-1",
						TargetNodeId: "node-2",
						Label:        "maps_to",
						Temporal:     &pb.TemporalAttributes{Confidence: 0.8, DeterminedBy: "job-8"},
					},
					{
						SourceNodeId: "node-2",
						TargetNodeId: "node-3",
						Label:        "depends_on",
					},
				},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(resp.Msg.CreatedCount).To(Equal(int32(2)))
			Expect(resp.Msg.EdgeIds).To(HaveLen(2))
			Expect(capturedEdges).To(HaveLen(2))
			Expect(capturedEdges[0].SourceID).To(Equal("node-1"))
			Expect(capturedEdges[0].TargetID).To(Equal("node-2"))
			Expect(capturedEdges[0].Edge.Label).To(Equal("maps_to"))
			Expect(capturedEdges[0].Edge.Confidence).To(Equal(0.8))
			Expect(capturedEdges[0].Edge.DeterminedBy).To(Equal("job-8"))
			Expect(capturedEdges[1].Edge.Confidence).To(BeZero())
			Expect(capturedEdges[1].Edge.DeterminedBy).To(BeEmpty())
			Expect(capturedEdges[1].SourceID).To(Equal("node-2"))
			Expect(capturedEdges[1].TargetID).To(Equal("node-3"))
			Expect(capturedEdges[1].Edge.Label).To(Equal("depends_on"))
		})

		It("returns only the error when the driver rejects the batch", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			mockGraph.bulkCreateEdgesFunc = func(ctx context.Context, tenant string, edges []graphdb.BulkEdge) ([]string, error) {
				return nil, graphdb.ErrNodeNotFound
			}

			resp, err := svc.BulkCreateEdges(ctx, connect.NewRequest(&pb.BulkCreateEdgesRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Edges: []*pb.CreateEdgeRequest{
					{SourceNodeId: "node-1", TargetNodeId: "node-2", Label: "maps_to"},
					{SourceNodeId: "node-3", TargetNodeId: "node-4", Label: "maps_to"},
				},
			}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
			var ce *connect.Error
			Expect(errors.As(err, &ce)).To(BeTrue())
			Expect(ce.Message()).To(Equal(graphdb.ErrNodeNotFound.Error()))
			Expect(resp).To(BeNil(), "connect discards a response returned with an error, so the handler must not build one")
		})

		Context("edge count limit", func() {
			var driverCalls int

			edgesOf := func(n int) []*pb.CreateEdgeRequest {
				edges := make([]*pb.CreateEdgeRequest, n)
				for i := range edges {
					edges[i] = &pb.CreateEdgeRequest{SourceNodeId: "node-1", TargetNodeId: "node-2", Label: "maps_to"}
				}
				return edges
			}
			bulk := func(n int) (*connect.Response[pb.BulkCreateEdgesResponse], error) {
				return svc.BulkCreateEdges(testspecs.SetupTenantContext("test-tenant"), connect.NewRequest(&pb.BulkCreateEdgesRequest{
					TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
					Edges:         edgesOf(n),
				}))
			}

			BeforeEach(func() {
				driverCalls = 0
				mockGraph.bulkCreateEdgesFunc = func(_ context.Context, _ string, edges []graphdb.BulkEdge) ([]string, error) {
					driverCalls++
					return make([]string, len(edges)), nil
				}
			})

			It("accepts a request at the configured limit", func() {
				svc = graph.New(mockGraph, mockVectors, nil, graph.WithMaxBulkEdges(3))
				resp, err := bulk(3)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.Msg.CreatedCount).To(Equal(int32(3)))
				Expect(driverCalls).To(Equal(1))
			})

			It("rejects a request over the configured limit before reaching the graph", func() {
				svc = graph.New(mockGraph, mockVectors, nil, graph.WithMaxBulkEdges(3))
				resp, err := bulk(4)
				Expect(resp).To(BeNil())
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
				Expect(err.Error()).To(ContainSubstring("request has 4 edges"))
				Expect(err.Error()).To(ContainSubstring("limit of 3 (graph.max_bulk_edges)"))
				Expect(err.Error()).To(ContainSubstring("split the edges into requests of at most 3"))
				Expect(driverCalls).To(BeZero())
			})

			DescribeTable("ignores a non-positive limit and keeps config.DefaultGraphMaxBulkEdges",
				func(n int) {
					svc = graph.New(mockGraph, mockVectors, nil, graph.WithMaxBulkEdges(n))
					_, err := bulk(config.DefaultGraphMaxBulkEdges)
					Expect(err).NotTo(HaveOccurred())

					_, err = bulk(config.DefaultGraphMaxBulkEdges + 1)
					Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
					Expect(err.Error()).To(ContainSubstring(fmt.Sprintf("limit of %d", config.DefaultGraphMaxBulkEdges)))
				},
				Entry("zero", 0),
				Entry("negative", -1),
			)

			It("applies config.DefaultGraphMaxBulkEdges when no limit is configured", func() {
				resp, err := bulk(config.DefaultGraphMaxBulkEdges)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.Msg.CreatedCount).To(Equal(int32(config.DefaultGraphMaxBulkEdges)))

				_, err = bulk(config.DefaultGraphMaxBulkEdges + 1)
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
				Expect(driverCalls).To(Equal(1))
			})
		})
	})

	Describe("SupersedeFact", func() {
		It("rejects missing tenant context", func() {
			resp, err := svc.SupersedeFact(context.Background(), connect.NewRequest(&pb.SupersedeFactRequest{}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("rejects missing target", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			resp, err := svc.SupersedeFact(ctx, connect.NewRequest(&pb.SupersedeFactRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("node_id or edge_id is required"))
		})

		It("supersedes a node by ID", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			var capturedReq graphdb.SupersedeRequest

			mockGraph.supersedeFactFunc = func(ctx context.Context, tenant string, req graphdb.SupersedeRequest) (bool, error) {
				capturedReq = req
				return true, nil
			}

			resp, err := svc.SupersedeFact(ctx, connect.NewRequest(&pb.SupersedeFactRequest{
				TenantContext:     &pb.TenantContext{TenantId: "test-tenant"},
				Target:            &pb.SupersedeFactRequest_NodeId{NodeId: "node-1"},
				SupersededByJobId: "job-123",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(resp.Msg.Updated).To(BeTrue())
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

			resp, err := svc.SupersedeFact(ctx, connect.NewRequest(&pb.SupersedeFactRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Target:        &pb.SupersedeFactRequest_EdgeId{EdgeId: "edge-1"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(resp.Msg.Updated).To(BeTrue())
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

			resp, err := svc.SupersedeFact(ctx, connect.NewRequest(&pb.SupersedeFactRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Target:        &pb.SupersedeFactRequest_NodeId{NodeId: "node-1"},
				SupersededAt:  timestamppb.New(specificTime),
			}))
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

			resp, err := svc.SupersedeFact(ctx, connect.NewRequest(&pb.SupersedeFactRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Target:        &pb.SupersedeFactRequest_NodeId{NodeId: "node-1"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(capturedReq.SupersededAt).To(BeTemporally("~", now, 2*time.Second))
		})

		It("propagates graphdb errors", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			mockGraph.supersedeFactFunc = func(ctx context.Context, tenant string, req graphdb.SupersedeRequest) (bool, error) {
				return false, graphdb.ErrNodeNotFound
			}

			resp, err := svc.SupersedeFact(ctx, connect.NewRequest(&pb.SupersedeFactRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Target:        &pb.SupersedeFactRequest_NodeId{NodeId: "nonexistent"},
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
		})
	})
})

var _ = Describe("Edge confidence validation", func() {
	const tenantID = "test-tenant"
	var (
		ctx context.Context
		db  graphdb.GraphDB
		svc *graph.Service
	)

	BeforeEach(func() {
		ctx = testspecs.SetupTenantContext(tenantID)
		db = memdriver.New()
		Expect(db.CreateGraph(ctx, tenantID)).To(Succeed())
		for _, id := range []string{"a", "b"} {
			Expect(db.CreateNode(ctx, tenantID, graphdb.Node{ID: id, Label: "Control", ValidFrom: time.Now()})).To(Succeed())
		}
		svc = graph.New(db, &mockVectorDB{}, nil)
	})

	edgeReq := func(confidence float32) *pb.CreateEdgeRequest {
		return &pb.CreateEdgeRequest{
			TenantContext: &pb.TenantContext{TenantId: tenantID},
			SourceNodeId:  "a",
			TargetNodeId:  "b",
			Label:         "MAPS",
			Temporal:      &pb.TemporalAttributes{ValidFrom: timestamppb.Now(), Confidence: confidence},
		}
	}
	storedEdges := func() []graphdb.Relationship {
		rels, err := db.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{})
		Expect(err).NotTo(HaveOccurred())
		return rels
	}
	invalid := []any{
		Entry("NaN", float32(math.NaN())),
		Entry("+Inf", float32(math.Inf(1))),
		Entry("-Inf", float32(math.Inf(-1))),
		Entry("-0.1", float32(-0.1)),
		Entry("1.1", float32(1.1)),
	}

	DescribeTable("CreateEdge rejects a confidence that is not a finite number in [0, 1] and stores nothing",
		append([]any{func(c float32) {
			_, err := svc.CreateEdge(ctx, connect.NewRequest(edgeReq(c)))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("temporal.confidence"))
			Expect(err.Error()).To(ContainSubstring("from 0 to 1 inclusive"))
			Expect(storedEdges()).To(BeEmpty())
		}}, invalid...)...,
	)

	DescribeTable("BulkCreateEdges rejects the whole batch for one bad confidence, naming its index, and stores nothing",
		append([]any{func(c float32) {
			_, err := svc.BulkCreateEdges(ctx, connect.NewRequest(&pb.BulkCreateEdgesRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				Edges:         []*pb.CreateEdgeRequest{edgeReq(0.5), edgeReq(c)},
			}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("[1]"))
			Expect(err.Error()).To(ContainSubstring("temporal.confidence"))
			Expect(storedEdges()).To(BeEmpty())
		}}, invalid...)...,
	)

	It("prints the rejected confidence as the client sent it", func() {
		_, err := svc.CreateEdge(ctx, connect.NewRequest(edgeReq(-0.1)))
		Expect(err.Error()).To(ContainSubstring("temporal.confidence -0.1 is invalid"))
	})

	DescribeTable("accepts the inclusive bounds and stores the edge",
		func(c float32) {
			_, err := svc.CreateEdge(ctx, connect.NewRequest(edgeReq(c)))
			Expect(err).NotTo(HaveOccurred())
			rels := storedEdges()
			Expect(rels).To(HaveLen(1))
			Expect(rels[0].Edge.Confidence).To(Equal(float64(c)))
		},
		Entry("0", float32(0)),
		Entry("1", float32(1)),
	)

	It("rewords a reserved property key error in proto field names", func() {
		req := edgeReq(0.5)
		req.Properties = map[string]string{"confidence": "0.9"}
		_, err := svc.CreateEdge(ctx, connect.NewRequest(req))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(err.Error()).To(ContainSubstring(`properties["confidence"] is reserved: set temporal.confidence instead`))
		Expect(err.Error()).NotTo(ContainSubstring("Edge.Confidence"))
		Expect(storedEdges()).To(BeEmpty())
	})

	It("keeps the bulk edge index when rewording a reserved property key error", func() {
		bad := edgeReq(0.5)
		bad.Properties = map[string]string{"valid_from": "x"}
		_, err := svc.BulkCreateEdges(ctx, connect.NewRequest(&pb.BulkCreateEdgesRequest{
			TenantContext: &pb.TenantContext{TenantId: tenantID},
			Edges:         []*pb.CreateEdgeRequest{edgeReq(0.5), bad},
		}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(err.Error()).To(ContainSubstring(`[1]: properties["valid_from"] is reserved: set temporal.valid_from instead`))
		Expect(storedEdges()).To(BeEmpty())
	})
})

// createNodeSpy records the ID of every node the RPC hands to the driver,
// then stores it in the embedded memdriver. Every other method goes straight
// to the memdriver.
type createNodeSpy struct {
	graphdb.GraphDB
	attempted []string
}

func (s *createNodeSpy) CreateNode(ctx context.Context, tenant string, n graphdb.Node) error {
	s.attempted = append(s.attempted, n.ID)
	return s.GraphDB.CreateNode(ctx, tenant, n)
}

var _ = Describe("CreateNode reserved property keys", func() {
	const tenantID = "test-tenant"

	// hints is what the RPC must tell a client instead of the driver's
	// Go-field wording, one entry per graphdb.ReservedNodeKeys key.
	hints := map[string]string{
		"id":              "the server generates the ID; rename the property",
		"valid_from":      "set temporal.valid_from instead",
		"valid_to":        "set temporal.valid_to instead",
		"created_by":      "set internally by the pipeline; rename the property",
		"creation_method": "set internally by the pipeline; rename the property",
		"superseded_by":   "call SupersedeFact with superseded_by_job_id instead",
	}

	var (
		ctx context.Context
		db  graphdb.GraphDB
		spy *createNodeSpy
		svc *graph.Service
	)

	BeforeEach(func() {
		ctx = testspecs.SetupTenantContext(tenantID)
		db = memdriver.New()
		Expect(db.CreateGraph(ctx, tenantID)).To(Succeed())
		// The RPC generates the node ID, so record it on its way to the
		// driver to prove afterwards that nothing was stored under it.
		spy = &createNodeSpy{GraphDB: db}
		svc = graph.New(spy, &mockVectorDB{}, nil)
	})

	It("covers every reserved node key", func() {
		Expect(hints).To(HaveLen(len(graphdb.ReservedNodeKeys())))
		for key := range graphdb.ReservedNodeKeys() {
			Expect(hints).To(HaveKey(key))
		}
	})

	entries := []any{}
	for key, hint := range hints {
		entries = append(entries, Entry(key, key, hint))
	}

	DescribeTable("rejects the key with a message in proto field names and stores nothing",
		append([]any{func(key, hint string) {
			_, err := svc.CreateNode(ctx, connect.NewRequest(&pb.CreateNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				Label:         "Control",
				Properties:    map[string]string{key: "x"},
			}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			var ce *connect.Error
			Expect(errors.As(err, &ce)).To(BeTrue())
			Expect(ce.Message()).To(Equal(fmt.Sprintf("properties[%q] is reserved: %s", key, hint)))
			Expect(ce.Message()).NotTo(ContainSubstring("Node."), "no Go field name may reach the client")

			Expect(spy.attempted).To(HaveLen(1), "the RPC must reach the driver exactly once")
			_, err = db.GetNode(ctx, tenantID, spy.attempted[0])
			Expect(err).To(MatchError(graphdb.ErrNodeNotFound), "the rejected node must not be stored")
		}}, entries...)...,
	)
})

var _ = Describe("BulkCreateEdges error details", func() {
	const tenantID = "test-tenant"
	var (
		ctx context.Context
		db  graphdb.GraphDB
		svc *graph.Service
	)

	BeforeEach(func() {
		ctx = testspecs.SetupTenantContext(tenantID)
		db = memdriver.New()
		Expect(db.CreateGraph(ctx, tenantID)).To(Succeed())
		for _, id := range []string{"a", "b"} {
			Expect(db.CreateNode(ctx, tenantID, graphdb.Node{ID: id, Label: "Control", ValidFrom: time.Now()})).To(Succeed())
		}
		svc = graph.New(db, &mockVectorDB{}, nil)
	})

	DescribeTable("names the failing edge in the RPC error message and stores nothing",
		func(bad *pb.CreateEdgeRequest, code connect.Code, message string) {
			good := &pb.CreateEdgeRequest{SourceNodeId: "a", TargetNodeId: "b", Label: "MAPS"}
			resp, err := svc.BulkCreateEdges(ctx, connect.NewRequest(&pb.BulkCreateEdgesRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				Edges:         []*pb.CreateEdgeRequest{good, bad},
			}))
			Expect(connect.CodeOf(err)).To(Equal(code))
			var ce *connect.Error
			Expect(errors.As(err, &ce)).To(BeTrue())
			Expect(ce.Message()).To(Equal(message))

			Expect(resp).To(BeNil())

			rels, qerr := db.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{})
			Expect(qerr).NotTo(HaveOccurred())
			Expect(rels).To(BeEmpty(), "the batch is all-or-nothing, so edge [0] must not be stored either")
		},
		Entry("a missing endpoint, worded by the driver",
			&pb.CreateEdgeRequest{SourceNodeId: "a", TargetNodeId: "missing", Label: "MAPS"},
			connect.CodeNotFound,
			`bulk create edges [1]: target: node "missing": node not found`),
		Entry("a reserved property key, reworded in proto field names",
			&pb.CreateEdgeRequest{SourceNodeId: "a", TargetNodeId: "b", Label: "MAPS", Properties: map[string]string{"valid_from": "x"}},
			connect.CodeInvalidArgument,
			`bulk create edges [1]: properties["valid_from"] is reserved: set temporal.valid_from instead`),
	)
})
