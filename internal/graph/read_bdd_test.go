package graph_test

import (
	"context"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
)

var _ = Describe("Read RPCs", func() {
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

	Describe("GetNode", func() {
		It("rejects missing tenant context", func() {
			resp, err := svc.GetNode(context.Background(), connect.NewRequest(&pb.GetNodeRequest{}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("tenant_context is required"))
		})

		It("rejects missing node_id", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			resp, err := svc.GetNode(ctx, connect.NewRequest(&pb.GetNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err.Error()).To(ContainSubstring("node_id is required"))
		})

		It("returns NotFound when node does not exist", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			mockGraph.getNodeFunc = func(ctx context.Context, tenant, nodeID string) (*graphdb.Node, error) {
				return nil, graphdb.ErrNodeNotFound
			}

			resp, err := svc.GetNode(ctx, connect.NewRequest(&pb.GetNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				NodeId:        "nonexistent",
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
		})

		It("returns the node when found", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			now := time.Now().UTC()
			mockGraph.getNodeFunc = func(ctx context.Context, tenant, nodeID string) (*graphdb.Node, error) {
				return &graphdb.Node{
					ID:    "node-1",
					Label: "Control",
					Properties: map[string]any{
						"title": "AC-1",
					},
					ValidFrom: now,
					CreatedBy: "test-user",
				}, nil
			}

			resp, err := svc.GetNode(ctx, connect.NewRequest(&pb.GetNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				NodeId:        "node-1",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(resp.Msg.Node.NodeId).To(Equal("node-1"))
			Expect(resp.Msg.Node.Label).To(Equal("Control"))
			Expect(resp.Msg.Node.Properties["title"]).To(Equal("AC-1"))
		})
	})

	Describe("GetEdge", func() {
		It("rejects missing tenant context", func() {
			resp, err := svc.GetEdge(context.Background(), connect.NewRequest(&pb.GetEdgeRequest{}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("rejects missing edge_id", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			resp, err := svc.GetEdge(ctx, connect.NewRequest(&pb.GetEdgeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("returns NotFound when edge does not exist", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			mockGraph.getEdgeFunc = func(ctx context.Context, tenant, edgeID string) (*graphdb.EdgeWithEndpoints, error) {
				return nil, graphdb.ErrEdgeNotFound
			}

			resp, err := svc.GetEdge(ctx, connect.NewRequest(&pb.GetEdgeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				EdgeId:        "nonexistent",
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
		})

		It("returns the edge when found", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			now := time.Now().UTC()
			mockGraph.getEdgeFunc = func(ctx context.Context, tenant, edgeID string) (*graphdb.EdgeWithEndpoints, error) {
				return &graphdb.EdgeWithEndpoints{
					Edge: graphdb.Edge{
						ID:    "edge-1",
						Label: "maps_to",
						Properties: map[string]any{
							"confidence": 0.95,
						},
						ValidFrom: now,
					},
					SourceID: "node-1",
					TargetID: "node-2",
				}, nil
			}

			resp, err := svc.GetEdge(ctx, connect.NewRequest(&pb.GetEdgeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				EdgeId:        "edge-1",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(resp.Msg.Edge.EdgeId).To(Equal("edge-1"))
			Expect(resp.Msg.Edge.Label).To(Equal("maps_to"))
			Expect(resp.Msg.Edge.SourceNodeId).To(Equal("node-1"))
			Expect(resp.Msg.Edge.TargetNodeId).To(Equal("node-2"))
		})
	})

	Describe("Traverse", func() {
		It("rejects missing tenant context", func() {
			resp, err := svc.Traverse(context.Background(), connect.NewRequest(&pb.TraverseRequest{}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("rejects missing start_node_id", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			resp, err := svc.Traverse(ctx, connect.NewRequest(&pb.TraverseRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("delegates to graph.Traverse with correct parameters", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			var capturedQuery graphdb.TraversalQuery

			mockGraph.traverseFunc = func(ctx context.Context, tenant string, query graphdb.TraversalQuery) ([]graphdb.Path, error) {
				capturedQuery = query
				return []graphdb.Path{
					{
						Nodes: []graphdb.Node{
							{ID: "node-1", Label: "Control"},
							{ID: "node-2", Label: "Control"},
						},
						Edges: []graphdb.Edge{
							{ID: "edge-1", Label: "maps_to"},
						},
					},
				}, nil
			}

			resp, err := svc.Traverse(ctx, connect.NewRequest(&pb.TraverseRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				StartNodeId:   "node-1",
				Direction:     pb.TraversalDirection_TRAVERSAL_DIRECTION_OUTBOUND,
				EdgeLabels:    []string{"maps_to"},
				MaxDepth:      2,
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(capturedQuery.StartNode).To(Equal("node-1"))
			Expect(capturedQuery.Direction).To(Equal("outbound"))
			Expect(capturedQuery.EdgeLabels).To(Equal([]string{"maps_to"}))
			Expect(capturedQuery.MaxDepth).To(Equal(2))
			Expect(resp.Msg.Nodes).To(HaveLen(2))
			Expect(resp.Msg.Edges).To(HaveLen(1))
		})

		It("propagates graphdb errors", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			mockGraph.traverseFunc = func(ctx context.Context, tenant string, query graphdb.TraversalQuery) ([]graphdb.Path, error) {
				return nil, graphdb.ErrGraphNotFound
			}
			resp, err := svc.Traverse(ctx, connect.NewRequest(&pb.TraverseRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				StartNodeId:   "node-1",
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
		})

		It("passes as_of to graphdb when set", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			asOfTime := time.Date(2025, 6, 15, 10, 30, 0, 0, time.UTC)
			var capturedQuery graphdb.TraversalQuery

			mockGraph.traverseFunc = func(ctx context.Context, tenant string, query graphdb.TraversalQuery) ([]graphdb.Path, error) {
				capturedQuery = query
				return []graphdb.Path{}, nil
			}

			resp, err := svc.Traverse(ctx, connect.NewRequest(&pb.TraverseRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				StartNodeId:   "node-1",
				AsOf:          timestamppb.New(asOfTime),
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(capturedQuery.AsOf).NotTo(BeNil())
			Expect(*capturedQuery.AsOf).To(Equal(asOfTime))
		})

		It("leaves AsOf nil when as_of is not set", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			var capturedQuery graphdb.TraversalQuery

			mockGraph.traverseFunc = func(ctx context.Context, tenant string, query graphdb.TraversalQuery) ([]graphdb.Path, error) {
				capturedQuery = query
				return []graphdb.Path{}, nil
			}

			resp, err := svc.Traverse(ctx, connect.NewRequest(&pb.TraverseRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				StartNodeId:   "node-1",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(capturedQuery.AsOf).To(BeNil())
		})
	})

	Describe("Query", func() {
		It("rejects missing tenant context", func() {
			resp, err := svc.Query(context.Background(), connect.NewRequest(&pb.QueryRequest{}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("rejects non-admin callers", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			resp, err := svc.Query(ctx, connect.NewRequest(&pb.QueryRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Cypher:        "MATCH (n) RETURN n",
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
		})

		It("rejects missing cypher query", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			ctx = graph.ExportContextWithIdentity(ctx, &authn.Identity{
				Subject:  "admin@test.com",
				TenantID: "test-tenant",
				Roles:    []string{authn.RoleAdmin},
			})

			resp, err := svc.Query(ctx, connect.NewRequest(&pb.QueryRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("executes query for admin user", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			ctx = graph.ExportContextWithIdentity(ctx, &authn.Identity{
				Subject:  "admin@test.com",
				TenantID: "test-tenant",
				Roles:    []string{authn.RoleAdmin},
			})

			mockGraph.executeQueryFunc = func(ctx context.Context, tenant, cypher string, params map[string]string) ([]graphdb.QueryRow, error) {
				return []graphdb.QueryRow{
					{
						Values: []graphdb.QueryValue{
							{Type: graphdb.QueryValueScalar, ScalarVal: "test"},
						},
					},
				}, nil
			}

			resp, err := svc.Query(ctx, connect.NewRequest(&pb.QueryRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Cypher:        "MATCH (n) RETURN n.id",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(resp.Msg.RowCount).To(Equal(int32(1)))
		})

		It("rejects authenticated non-admin callers with PermissionDenied", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			ctx = graph.ExportContextWithIdentity(ctx, &authn.Identity{
				Subject:  "user@test.com",
				TenantID: "test-tenant",
				Roles:    []string{"viewer"},
			})
			resp, err := svc.Query(ctx, connect.NewRequest(&pb.QueryRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Cypher:        "MATCH (n) RETURN n",
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
		})

		It("propagates graphdb errors", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			ctx = graph.ExportContextWithIdentity(ctx, &authn.Identity{
				Subject:  "admin@test.com",
				TenantID: "test-tenant",
				Roles:    []string{authn.RoleAdmin},
			})
			mockGraph.executeQueryFunc = func(ctx context.Context, tenant, cypher string, params map[string]string) ([]graphdb.QueryRow, error) {
				return nil, graphdb.ErrGraphNotFound
			}
			resp, err := svc.Query(ctx, connect.NewRequest(&pb.QueryRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Cypher:        "MATCH (n) RETURN n",
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
		})
	})

	Describe("SimilaritySearch", func() {
		It("rejects missing tenant context", func() {
			resp, err := svc.SimilaritySearch(context.Background(), connect.NewRequest(&pb.SimilaritySearchRequest{}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("rejects missing query", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			resp, err := svc.SimilaritySearch(ctx, connect.NewRequest(&pb.SimilaritySearchRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("performs vector similarity search", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			mockVectors.findSimilarFunc = func(ctx context.Context, tenantID string, query vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
				return []vectordb.SimilarityResult{
					{ControlID: "control-1", Similarity: 0.95, Metadata: map[string]any{}},
					{ControlID: "control-2", Similarity: 0.85, Metadata: map[string]any{}},
				}, nil
			}

			resp, err := svc.SimilaritySearch(ctx, connect.NewRequest(&pb.SimilaritySearchRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Query: &pb.SimilaritySearchRequest_QueryEmbedding{
					QueryEmbedding: &pb.EmbeddingQuery{
						Embeddings: []float32{0.1, 0.2, 0.3},
					},
				},
				Limit: 10,
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(resp.Msg.Matches).To(HaveLen(2))
			Expect(resp.Msg.Matches[0].Node.NodeId).To(Equal("control-1"))
			Expect(resp.Msg.Matches[0].SimilarityScore).To(BeNumerically("~", 0.95, 0.01))
		})

		It("performs control-based similarity search", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			mockVectors.findSimilarFunc = func(ctx context.Context, tenantID string, query vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
				Expect(query.CatalogID).To(Equal("control-1"))
				return []vectordb.SimilarityResult{
					{ControlID: "control-2", Similarity: 0.92, Metadata: map[string]any{}},
				}, nil
			}

			resp, err := svc.SimilaritySearch(ctx, connect.NewRequest(&pb.SimilaritySearchRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Query:         &pb.SimilaritySearchRequest_ControlId{ControlId: "control-1"},
				Limit:         5,
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(resp.Msg.Matches).To(HaveLen(1))
		})

		It("defaults limit to 10 when not specified", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			var capturedQuery vectordb.FindSimilarQuery

			mockVectors.findSimilarFunc = func(ctx context.Context, tenantID string, query vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
				capturedQuery = query
				return nil, nil
			}

			_, _ = svc.SimilaritySearch(ctx, connect.NewRequest(&pb.SimilaritySearchRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Query: &pb.SimilaritySearchRequest_QueryEmbedding{
					QueryEmbedding: &pb.EmbeddingQuery{Embeddings: []float32{0.1}},
				},
			}))
			Expect(capturedQuery.Limit).To(Equal(10))
		})

		It("returns FailedPrecondition when vectorDB is nil", func() {
			nilVectorSvc := graph.New(mockGraph, nil, nil)
			ctx := testspecs.SetupTenantContext("test-tenant")
			resp, err := nilVectorSvc.SimilaritySearch(ctx, connect.NewRequest(&pb.SimilaritySearchRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Query: &pb.SimilaritySearchRequest_QueryEmbedding{
					QueryEmbedding: &pb.EmbeddingQuery{Embeddings: []float32{0.1}},
				},
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		})

		It("rejects empty embedding", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			resp, err := svc.SimilaritySearch(ctx, connect.NewRequest(&pb.SimilaritySearchRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Query: &pb.SimilaritySearchRequest_QueryEmbedding{
					QueryEmbedding: &pb.EmbeddingQuery{Embeddings: []float32{}},
				},
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("propagates vectordb errors", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			mockVectors.findSimilarFunc = func(ctx context.Context, tenantID string, query vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
				return nil, vectordb.ErrNotFound
			}
			resp, err := svc.SimilaritySearch(ctx, connect.NewRequest(&pb.SimilaritySearchRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Query: &pb.SimilaritySearchRequest_QueryEmbedding{
					QueryEmbedding: &pb.EmbeddingQuery{Embeddings: []float32{0.1, 0.2}},
				},
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
		})
	})

	Describe("TemporalQuery", func() {
		It("rejects missing tenant context", func() {
			resp, err := svc.TemporalQuery(context.Background(), connect.NewRequest(&pb.TemporalQueryRequest{}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("rejects non-admin callers", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			resp, err := svc.TemporalQuery(ctx, connect.NewRequest(&pb.TemporalQueryRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Cypher:        "MATCH (n) RETURN n",
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
		})

		It("rejects missing cypher query", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			ctx = graph.ExportContextWithIdentity(ctx, &authn.Identity{
				Subject:  "admin@test.com",
				TenantID: "test-tenant",
				Roles:    []string{authn.RoleAdmin},
			})

			resp, err := svc.TemporalQuery(ctx, connect.NewRequest(&pb.TemporalQueryRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("rejects missing as_of timestamp", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			ctx = graph.ExportContextWithIdentity(ctx, &authn.Identity{
				Subject:  "admin@test.com",
				TenantID: "test-tenant",
				Roles:    []string{authn.RoleAdmin},
			})

			resp, err := svc.TemporalQuery(ctx, connect.NewRequest(&pb.TemporalQueryRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Cypher:        "MATCH (n) RETURN n",
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("executes temporal query for admin user", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			ctx = graph.ExportContextWithIdentity(ctx, &authn.Identity{
				Subject:  "admin@test.com",
				TenantID: "test-tenant",
				Roles:    []string{authn.RoleAdmin},
			})

			var capturedParams map[string]string
			mockGraph.executeQueryFunc = func(ctx context.Context, tenant, cypher string, params map[string]string) ([]graphdb.QueryRow, error) {
				capturedParams = params
				return []graphdb.QueryRow{
					{Values: []graphdb.QueryValue{{Type: graphdb.QueryValueScalar, ScalarVal: "test"}}},
				}, nil
			}

			resp, err := svc.TemporalQuery(ctx, connect.NewRequest(&pb.TemporalQueryRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Cypher:        "MATCH (n) WHERE n.valid_from <= $as_of RETURN n",
				AsOf:          timestamppb.Now(),
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(resp.Msg.RowCount).To(Equal(int32(1)))
			Expect(capturedParams).To(HaveKey("as_of"))
		})
	})
})
