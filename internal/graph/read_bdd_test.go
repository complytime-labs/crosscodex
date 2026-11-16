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
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
	"github.com/complytime-labs/crosscodex/pkg/vectordb/vectordbtest"
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

	Describe("invalid requests", func() {
		tenantCtx := func() context.Context { return testspecs.SetupTenantContext("test-tenant") }
		adminCtx := func() context.Context {
			return graph.ExportContextWithIdentity(tenantCtx(), &authn.Identity{
				Subject:  "admin@test.com",
				TenantID: "test-tenant",
				Roles:    []string{authn.RoleAdmin},
			})
		}
		tc := &pb.TenantContext{TenantId: "test-tenant"}

		DescribeTable("are rejected with the expected code and no response",
			func(newCtx func() context.Context, call func(context.Context, *graph.Service) (any, error), wantCode connect.Code, wantMsg string) {
				resp, err := call(newCtx(), svc)
				Expect(resp).To(BeNil())
				Expect(connect.CodeOf(err)).To(Equal(wantCode))
				Expect(err).To(MatchError(ContainSubstring(wantMsg)))
			},
			Entry("GetNode without tenant context", context.Background,
				func(ctx context.Context, s *graph.Service) (any, error) {
					return s.GetNode(ctx, connect.NewRequest(&pb.GetNodeRequest{}))
				}, connect.CodeInvalidArgument, "tenant_context is required"),
			Entry("GetNode without node_id", tenantCtx,
				func(ctx context.Context, s *graph.Service) (any, error) {
					return s.GetNode(ctx, connect.NewRequest(&pb.GetNodeRequest{TenantContext: tc}))
				}, connect.CodeInvalidArgument, "node_id is required"),
			Entry("GetEdge without tenant context", context.Background,
				func(ctx context.Context, s *graph.Service) (any, error) {
					return s.GetEdge(ctx, connect.NewRequest(&pb.GetEdgeRequest{}))
				}, connect.CodeInvalidArgument, "tenant_context is required"),
			Entry("GetEdge without edge_id", tenantCtx,
				func(ctx context.Context, s *graph.Service) (any, error) {
					return s.GetEdge(ctx, connect.NewRequest(&pb.GetEdgeRequest{TenantContext: tc}))
				}, connect.CodeInvalidArgument, "edge_id is required"),
			Entry("Traverse without tenant context", context.Background,
				func(ctx context.Context, s *graph.Service) (any, error) {
					return s.Traverse(ctx, connect.NewRequest(&pb.TraverseRequest{}))
				}, connect.CodeInvalidArgument, "tenant_context is required"),
			Entry("Traverse without start_node_id", tenantCtx,
				func(ctx context.Context, s *graph.Service) (any, error) {
					return s.Traverse(ctx, connect.NewRequest(&pb.TraverseRequest{TenantContext: tc}))
				}, connect.CodeInvalidArgument, "start_node_id is required"),
			Entry("Query without tenant context", context.Background,
				func(ctx context.Context, s *graph.Service) (any, error) {
					return s.Query(ctx, connect.NewRequest(&pb.QueryRequest{}))
				}, connect.CodeInvalidArgument, "tenant_context is required"),
			Entry("Query from an unauthenticated caller", tenantCtx,
				func(ctx context.Context, s *graph.Service) (any, error) {
					return s.Query(ctx, connect.NewRequest(&pb.QueryRequest{TenantContext: tc, Cypher: "MATCH (n) RETURN n"}))
				}, connect.CodeUnauthenticated, "not authenticated"),
			Entry("Query without cypher", adminCtx,
				func(ctx context.Context, s *graph.Service) (any, error) {
					return s.Query(ctx, connect.NewRequest(&pb.QueryRequest{TenantContext: tc}))
				}, connect.CodeInvalidArgument, "cypher is required"),
			Entry("TemporalQuery without tenant context", context.Background,
				func(ctx context.Context, s *graph.Service) (any, error) {
					return s.TemporalQuery(ctx, connect.NewRequest(&pb.TemporalQueryRequest{}))
				}, connect.CodeInvalidArgument, "tenant_context is required"),
			Entry("TemporalQuery from an unauthenticated caller", tenantCtx,
				func(ctx context.Context, s *graph.Service) (any, error) {
					return s.TemporalQuery(ctx, connect.NewRequest(&pb.TemporalQueryRequest{TenantContext: tc, Cypher: "MATCH (n) RETURN n"}))
				}, connect.CodeUnauthenticated, "not authenticated"),
			Entry("TemporalQuery without cypher", adminCtx,
				func(ctx context.Context, s *graph.Service) (any, error) {
					return s.TemporalQuery(ctx, connect.NewRequest(&pb.TemporalQueryRequest{TenantContext: tc}))
				}, connect.CodeInvalidArgument, "cypher is required"),
			Entry("TemporalQuery without as_of", adminCtx,
				func(ctx context.Context, s *graph.Service) (any, error) {
					return s.TemporalQuery(ctx, connect.NewRequest(&pb.TemporalQueryRequest{TenantContext: tc, Cypher: "MATCH (n) RETURN n"}))
				}, connect.CodeInvalidArgument, "as_of is required"),
		)
	})

	Describe("GetNode", func() {
		It("returns NotFound when node does not exist", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			svc := graph.New(newMemGraph(ctx, "test-tenant"), mockVectors, nil)

			resp, err := svc.GetNode(ctx, connect.NewRequest(&pb.GetNodeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				NodeId:        "nonexistent",
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
		})

		It("returns the node when found", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			db := newMemGraph(ctx, "test-tenant")
			Expect(db.CreateNode(ctx, "test-tenant", graphdb.Node{
				ID:         "node-1",
				Label:      "Control",
				Properties: map[string]any{"title": "AC-1"},
				ValidFrom:  time.Now().UTC(),
				CreatedBy:  "test-user",
			})).To(Succeed())
			svc := graph.New(db, mockVectors, nil)

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
		It("returns NotFound when edge does not exist", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			svc := graph.New(newMemGraph(ctx, "test-tenant"), mockVectors, nil)

			resp, err := svc.GetEdge(ctx, connect.NewRequest(&pb.GetEdgeRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				EdgeId:        "nonexistent",
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
		})

		It("returns the edge when found", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			db := newMemGraph(ctx, "test-tenant", "node-1", "node-2")
			Expect(db.CreateEdge(ctx, "test-tenant", "node-1", "node-2", graphdb.Edge{
				ID:         "edge-1",
				Label:      "maps_to",
				Properties: map[string]any{"method": "llm"},
				ValidFrom:  time.Now().UTC(),
			})).To(Succeed())
			svc := graph.New(db, mockVectors, nil)

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
		It("rejects an undefined direction without calling the graph", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			called := false
			mockGraph.traverseFunc = func(ctx context.Context, tenant string, query graphdb.TraversalQuery) ([]graphdb.Path, error) {
				called = true
				return nil, nil
			}
			resp, err := svc.Traverse(ctx, connect.NewRequest(&pb.TraverseRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				StartNodeId:   "node-1",
				Direction:     pb.TraversalDirection(99),
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(err).To(MatchError(ContainSubstring("unknown traversal direction 99")))
			Expect(err).To(MatchError(ContainSubstring(
				"use TRAVERSAL_DIRECTION_OUTBOUND, TRAVERSAL_DIRECTION_INBOUND or TRAVERSAL_DIRECTION_BOTH, or leave direction unset for outbound")),
				"the error must name the accepted directions")
			Expect(called).To(BeFalse(), "an undefined direction must not reach the graph")
		})

		DescribeTable("maps each defined direction to the graphdb direction",
			func(d pb.TraversalDirection, want string) {
				ctx := testspecs.SetupTenantContext("test-tenant")
				var capturedQuery graphdb.TraversalQuery
				mockGraph.traverseFunc = func(ctx context.Context, tenant string, query graphdb.TraversalQuery) ([]graphdb.Path, error) {
					capturedQuery = query
					return nil, nil
				}
				_, err := svc.Traverse(ctx, connect.NewRequest(&pb.TraverseRequest{
					TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
					StartNodeId:   "node-1",
					Direction:     d,
				}))
				Expect(err).NotTo(HaveOccurred())
				Expect(capturedQuery.Direction).To(Equal(want))
			},
			Entry("unspecified defaults to outbound", pb.TraversalDirection_TRAVERSAL_DIRECTION_UNSPECIFIED, "outbound"),
			Entry("outbound", pb.TraversalDirection_TRAVERSAL_DIRECTION_OUTBOUND, "outbound"),
			Entry("inbound", pb.TraversalDirection_TRAVERSAL_DIRECTION_INBOUND, "inbound"),
			Entry("both", pb.TraversalDirection_TRAVERSAL_DIRECTION_BOTH, "both"),
		)

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
		const tenantID = "test-tenant"
		tc := &pb.TenantContext{TenantId: tenantID}
		vec := []float32{0.1, 0.2, 0.3}

		var (
			ctx     context.Context
			ssvc    *graph.Service
			reader  *fakeControlsReader
			stored  []vectordb.Embedding
			found   map[string][]vectordb.SimilarityResult
			findErr map[string]error
			queries []vectordb.FindSimilarQuery
		)

		BeforeEach(func() {
			ctx = testspecs.SetupTenantContext(tenantID)
			reader = &fakeControlsReader{controls: []graph.CatalogControl{
				{ControlID: "c1/a", CatalogID: "c1"},
				{ControlID: "c1/b", CatalogID: "c1"},
			}}
			stored = []vectordb.Embedding{{CatalogID: "c1", ControlID: "c1/a", Model: "m", Vector: vec}}
			found = map[string][]vectordb.SimilarityResult{}
			findErr = map[string]error{}
			queries = nil
			fake := &vectordbtest.Fake{
				GetEmbeddingsFunc: func(_ context.Context, _, catalogID, controlID string) ([]vectordb.Embedding, error) {
					if catalogID == "c1" && controlID == "c1/a" {
						return stored, nil
					}
					return nil, vectordb.ErrNotFound
				},
				FindSimilarFunc: func(_ context.Context, _ string, q vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
					queries = append(queries, q)
					return found[q.CatalogID], findErr[q.CatalogID]
				},
			}
			ssvc = graph.New(&mockGraphDB{}, fake, nil, graph.WithControlsReader(reader))
		})

		// recordFind is a FindSimilarFunc that records the query, so a spec asserting
		// "nothing was searched" on its own fake cannot pass vacuously.
		recordFind := func(_ context.Context, _ string, q vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
			queries = append(queries, q)
			return nil, nil
		}

		byControl := func(id string, catalogs ...string) *pb.SimilaritySearchRequest {
			return &pb.SimilaritySearchRequest{TenantContext: tc, Query: &pb.SimilaritySearchRequest_ControlId{ControlId: id}, CatalogIds: catalogs}
		}
		byVector := func(v []float32, model string, catalogs ...string) *pb.SimilaritySearchRequest {
			return &pb.SimilaritySearchRequest{
				TenantContext: tc,
				Query:         &pb.SimilaritySearchRequest_QueryEmbedding{QueryEmbedding: &pb.EmbeddingQuery{Embeddings: v}},
				Model:         model,
				CatalogIds:    catalogs,
			}
		}
		search := func(req *pb.SimilaritySearchRequest) (*connect.Response[pb.SimilaritySearchResponse], error) {
			return ssvc.SimilaritySearch(ctx, connect.NewRequest(req))
		}
		// matched lists each match as "<catalog_id> <node_id>", in response order.
		matched := func(resp *connect.Response[pb.SimilaritySearchResponse]) []string {
			out := make([]string, 0, len(resp.Msg.GetMatches()))
			for _, m := range resp.Msg.GetMatches() {
				out = append(out, m.GetNode().GetProperties()["catalog_id"]+" "+m.GetNode().GetNodeId())
			}
			return out
		}
		message := func(err error) string {
			var ce *connect.Error
			Expect(errors.As(err, &ce)).To(BeTrue(), "expected a *connect.Error, got %v", err)
			return ce.Message()
		}

		It("searches the control's own catalog with its stored embedding and leaves the control out", func() {
			found["c1"] = []vectordb.SimilarityResult{
				{ControlID: "c1/a", Similarity: 1.0}, {ControlID: "c1/b", Similarity: 0.9}, {ControlID: "c1/c", Similarity: 0.8},
			}
			req := byControl("c1/a")
			req.Limit = 10

			resp, err := search(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(queries).To(Equal([]vectordb.FindSimilarQuery{{CatalogID: "c1", Model: "m", Vector: vec, Limit: 11}}))
			Expect(matched(resp)).To(Equal([]string{"c1 c1/b", "c1 c1/c"}))
			Expect(resp.Msg.GetMatches()[0].GetSimilarityScore()).To(BeNumerically("~", 0.9, 1e-6))
		})

		It("keeps the requested number of matches when the control's own row is dropped at the cutoff", func() {
			found["c1"] = []vectordb.SimilarityResult{
				{ControlID: "c1/a", Similarity: 1.0}, {ControlID: "c1/b", Similarity: 0.9}, {ControlID: "c1/c", Similarity: 0.8},
			}
			req := byControl("c1/a")
			req.Limit = 2

			resp, err := search(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(queries).To(Equal([]vectordb.FindSimilarQuery{{CatalogID: "c1", Model: "m", Vector: vec, Limit: 3}}))
			Expect(matched(resp)).To(Equal([]string{"c1 c1/b", "c1 c1/c"}))
		})

		It("merges the named catalogs by descending similarity and keeps the first limit", func() {
			found["c2"] = []vectordb.SimilarityResult{{ControlID: "c2/b", Similarity: 0.9}, {ControlID: "c2/a", Similarity: 0.6}}
			found["c3"] = []vectordb.SimilarityResult{{ControlID: "c3/a", Similarity: 0.8}, {ControlID: "c3/b", Similarity: 0.3}}
			req := byControl("c1/a", "c2", "c3")
			req.Limit = 2

			resp, err := search(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(matched(resp)).To(Equal([]string{"c2 c2/b", "c3 c3/a"}))
			Expect(queries).To(Equal([]vectordb.FindSimilarQuery{
				{CatalogID: "c2", Model: "m", Vector: vec, Limit: 3},
				{CatalogID: "c3", Model: "m", Vector: vec, Limit: 3},
			}))
		})

		It("orders equal similarities by catalog, then control ID", func() {
			found["c3"] = []vectordb.SimilarityResult{{ControlID: "a", Similarity: 0.9}}
			found["c2"] = []vectordb.SimilarityResult{{ControlID: "z", Similarity: 0.9}, {ControlID: "b", Similarity: 0.9}}

			resp, err := search(byVector(vec, "m", "c3", "c2"))
			Expect(err).NotTo(HaveOccurred())
			Expect(matched(resp)).To(Equal([]string{"c2 b", "c2 z", "c3 a"}))
		})

		It("leaves the control out of its own catalog only, keeping the same control ID found in another catalog", func() {
			found["c1"] = []vectordb.SimilarityResult{{ControlID: "c1/a", Similarity: 1.0}, {ControlID: "c1/b", Similarity: 0.5}}
			found["c2"] = []vectordb.SimilarityResult{{ControlID: "c1/a", Similarity: 0.8}}

			resp, err := search(byControl("c1/a", "c1", "c2"))
			Expect(err).NotTo(HaveOccurred())
			Expect(matched(resp)).To(Equal([]string{"c2 c1/a", "c1 c1/b"}))
		})

		It("searches each named catalog once, in first-occurrence order", func() {
			_, err := search(byControl("c1/a", "c2", "c3", "c2"))
			Expect(err).NotTo(HaveOccurred())
			Expect(queries).To(HaveLen(2))
			Expect(queries[0].CatalogID).To(Equal("c2"))
			Expect(queries[1].CatalogID).To(Equal("c3"))
		})

		It("counts distinct catalogs against graph.max_similarity_catalogs", func() {
			ssvc = graph.New(&mockGraphDB{}, &vectordbtest.Fake{FindSimilarFunc: func(_ context.Context, _ string, q vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
				queries = append(queries, q)
				return nil, nil
			}}, nil, graph.WithMaxSimilarityCatalogs(2))

			_, err := search(byVector(vec, "m", "c1", "c2", "c1", "c2"))
			Expect(err).NotTo(HaveOccurred())
			Expect(queries).To(HaveLen(2))
		})

		Context("with graph.max_similarity_limit", func() {
			limited := func(limit int32) *pb.SimilaritySearchRequest {
				req := byVector(vec, "m", "c1", "c2")
				req.Limit = limit
				return req
			}

			BeforeEach(func() {
				ssvc = graph.New(&mockGraphDB{}, &vectordbtest.Fake{FindSimilarFunc: func(_ context.Context, _ string, q vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
					queries = append(queries, q)
					return nil, nil
				}}, nil, graph.WithMaxSimilarityLimit(5))
			})

			It("rejects a limit above it before any vector query, naming the key and the maximum", func() {
				resp, err := search(limited(6))
				Expect(resp).To(BeNil())
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
				Expect(message(err)).To(And(ContainSubstring("limit 6"), ContainSubstring("graph.max_similarity_limit"), ContainSubstring("at most 5")))
				Expect(queries).To(BeEmpty())
			})

			It("rejects an int32-maximum limit", func() {
				_, err := search(limited(math.MaxInt32))
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
				Expect(queries).To(BeEmpty())
			})

			It("accepts a limit equal to it", func() {
				_, err := search(limited(5))
				Expect(err).NotTo(HaveOccurred())
				Expect(queries).To(HaveLen(2))
				Expect(queries[0].Limit).To(Equal(5))
			})
		})

		DescribeTable("asks the vector store for the default limit when limit is unset",
			func(req *pb.SimilaritySearchRequest, want int) {
				_, err := search(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(queries).To(HaveLen(1))
				Expect(queries[0].Limit).To(Equal(want))
			},
			Entry("10 plus the control's own row with control_id", byControl("c1/a"), 11),
			Entry("10 with query_embedding", byVector(vec, "m", "c1"), 10),
		)

		It("searches under the named model when the control has several", func() {
			other := []float32{0.4, 0.5, 0.6}
			stored = []vectordb.Embedding{
				{CatalogID: "c1", ControlID: "c1/a", Model: "m1", Vector: vec},
				{CatalogID: "c1", ControlID: "c1/a", Model: "m2", Vector: other},
			}
			req := byControl("c1/a")
			req.Model = "m2"

			_, err := search(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(queries).To(Equal([]vectordb.FindSimilarQuery{{CatalogID: "c1", Model: "m2", Vector: other, Limit: 11}}))
		})

		It("searches with the caller's vector and model in every named catalog", func() {
			found["c2"] = []vectordb.SimilarityResult{{ControlID: "x", Similarity: 0.4, Metadata: map[string]any{"title": "X"}}}

			resp, err := search(byVector(vec, "m", "c2"))
			Expect(err).NotTo(HaveOccurred())
			Expect(queries).To(Equal([]vectordb.FindSimilarQuery{{CatalogID: "c2", Model: "m", Vector: vec, Limit: 10}}))
			Expect(resp.Msg.GetMatches()[0].GetNode().GetProperties()).To(Equal(map[string]string{"title": "X", "catalog_id": "c2"}))
		})

		DescribeTable("rejects the request, names the fix and searches nothing",
			func(setup func(), req *pb.SimilaritySearchRequest, code connect.Code, msgs ...string) {
				setup()
				resp, err := search(req)
				Expect(resp).To(BeNil())
				Expect(connect.CodeOf(err)).To(Equal(code))
				for _, m := range msgs {
					Expect(message(err)).To(ContainSubstring(m))
				}
				Expect(queries).To(BeEmpty())
			},
			Entry("without tenant context", func() {}, &pb.SimilaritySearchRequest{}, connect.CodeInvalidArgument, "tenant_context is required"),
			Entry("without a query", func() {}, &pb.SimilaritySearchRequest{TenantContext: tc}, connect.CodeInvalidArgument, "query_embedding or control_id is required"),
			Entry("without a vector store", func() { ssvc = graph.New(&mockGraphDB{}, nil, nil, graph.WithControlsReader(reader)) },
				byControl("c1/a"), connect.CodeFailedPrecondition, "vector search not configured"),
			Entry("with an empty control_id", func() {}, byControl(""), connect.CodeInvalidArgument, "control_id is required"),
			Entry("control_id without a controls reader", func() { ssvc = graph.New(&mockGraphDB{}, &vectordbtest.Fake{FindSimilarFunc: recordFind}, nil) },
				byControl("c1/a"), connect.CodeFailedPrecondition, "control lookup not configured"),
			Entry("control_id of an unknown control", func() {}, byControl("c1/zz"), connect.CodeNotFound, `control "c1/zz" not found`),
			Entry("control_id of a control with no stored embedding", func() {}, byControl("c1/b"), connect.CodeNotFound,
				`control "c1/b" has no stored embedding`, "run an analysis that includes its catalog"),
			Entry("a model the control has no embedding under",
				func() {
					stored = []vectordb.Embedding{{ControlID: "c1/a", Model: "m1", Vector: vec}, {ControlID: "c1/a", Model: "m2", Vector: vec}}
				},
				&pb.SimilaritySearchRequest{TenantContext: tc, Query: &pb.SimilaritySearchRequest_ControlId{ControlId: "c1/a"}, Model: "x"},
				connect.CodeNotFound, `"x"`, "m1", "m2"),
			Entry("no model for a control with several stored",
				func() {
					stored = []vectordb.Embedding{{ControlID: "c1/a", Model: "m1", Vector: vec}, {ControlID: "c1/a", Model: "m2", Vector: vec}}
				},
				byControl("c1/a"), connect.CodeInvalidArgument, "m1", "m2", "model"),
			Entry("query_embedding without catalog_ids", func() {}, byVector(vec, "m"), connect.CodeInvalidArgument,
				"query_embedding search needs catalog_ids: name the catalogs whose embeddings to search"),
			Entry("query_embedding without model", func() {}, byVector(vec, "", "c1"), connect.CodeInvalidArgument, "query_embedding search needs model"),
			Entry("query_embedding with an empty vector", func() {}, byVector(nil, "m", "c1"), connect.CodeInvalidArgument,
				"query_embedding is empty: send the vector to search with"),
			Entry("query_embedding with a NaN component", func() {}, byVector([]float32{0.1, float32(math.NaN())}, "m", "c1"),
				connect.CodeInvalidArgument, "query_embedding must be finite and non-zero"),
			Entry("query_embedding with an infinite component", func() {}, byVector([]float32{float32(math.Inf(1)), 0.1}, "m", "c1"),
				connect.CodeInvalidArgument, "query_embedding must be finite and non-zero"),
			Entry("query_embedding of all zeros", func() {}, byVector([]float32{0, 0, 0}, "m", "c1"),
				connect.CodeInvalidArgument, "query_embedding must be finite and non-zero"),
			Entry("an empty catalog_ids entry", func() {}, byControl("c1/a", "c2", ""), connect.CodeInvalidArgument, "catalog_ids[1] is empty"),
			Entry("more distinct catalogs than graph.max_similarity_catalogs",
				func() {
					ssvc = graph.New(&mockGraphDB{}, &vectordbtest.Fake{FindSimilarFunc: recordFind}, nil, graph.WithMaxSimilarityCatalogs(2))
				},
				byVector(vec, "m", "c1", "c2", "c3"), connect.CodeInvalidArgument, "graph.max_similarity_catalogs", "limit of 2"),
		)

		It("fails the whole search, with no partial result, when one catalog has no embeddings under the model", func() {
			found["c2"] = []vectordb.SimilarityResult{{ControlID: "c2/a", Similarity: 0.9}}
			findErr["c3"] = fmt.Errorf("find similar: %w", vectordb.ErrModelNotFound)

			resp, err := search(byControl("c1/a", "c2", "c3"))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
			Expect(message(err)).To(Equal(`catalog "c3" has no embeddings under model "m"`))
		})

		It("passes a vector width mismatch through as InvalidArgument", func() {
			findErr["c1"] = fmt.Errorf("find similar: query has 2 dimensions, catalog stores 3: %w", vectordb.ErrInvalidDimension)

			resp, err := search(byVector([]float32{0.1, 0.2}, "m", "c1"))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(message(err)).To(Equal(findErr["c1"].Error()))
		})

		DescribeTable("hides a lookup failure behind an internal error",
			func(setup func()) {
				setup()
				resp, err := search(byControl("c1/a"))
				Expect(resp).To(BeNil())
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
				Expect(message(err)).NotTo(ContainSubstring("SQLSTATE"))
				Expect(queries).To(BeEmpty())
			},
			Entry("reading the control", func() { reader.getErr = errors.New("read control: SQLSTATE 08006") }),
			Entry("reading its embeddings", func() {
				ssvc = graph.New(&mockGraphDB{}, &vectordbtest.Fake{
					FindSimilarFunc: recordFind,
					GetEmbeddingsFunc: func(context.Context, string, string, string) ([]vectordb.Embedding, error) {
						return nil, errors.New("get embeddings: SQLSTATE 08006")
					},
				}, nil, graph.WithControlsReader(reader))
			}),
		)

		It("records the form, model and catalog count on its span", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
			ssvc = graph.New(&mockGraphDB{}, &vectordbtest.Fake{FindSimilarFunc: func(context.Context, string, vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
				return nil, nil
			}}, nil, graph.WithTelemetry(tp.TracerProvider(), tp.MeterProvider()))

			_, err = search(byVector(vec, "m", "c1", "c2"))
			Expect(err).NotTo(HaveOccurred())

			span := telemetrytest.FindSpan(tp.GetSpans(), "graph.SimilaritySearch")
			Expect(span).NotTo(BeNil())
			v, ok := telemetrytest.SpanAttribute(span, "similarity.form")
			Expect(ok).To(BeTrue())
			Expect(v.AsString()).To(Equal("query_embedding"))
			v, ok = telemetrytest.SpanAttribute(span, "similarity.model")
			Expect(ok).To(BeTrue())
			Expect(v.AsString()).To(Equal("m"))
			v, ok = telemetrytest.SpanAttribute(span, "similarity.catalogs")
			Expect(ok).To(BeTrue())
			Expect(v.AsInt64()).To(Equal(int64(2)))
		})
	})

	Describe("TemporalQuery", func() {
		It("rejects authenticated non-admin callers with PermissionDenied", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			ctx = graph.ExportContextWithIdentity(ctx, &authn.Identity{
				Subject:  "user@test.com",
				TenantID: "test-tenant",
				Roles:    []string{"viewer"},
			})
			resp, err := svc.TemporalQuery(ctx, connect.NewRequest(&pb.TemporalQueryRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Cypher:        "MATCH (n) RETURN n",
				AsOf:          timestamppb.Now(),
			}))
			Expect(resp).To(BeNil())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
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

		It("encodes a whole-second as_of using the fixed-width graphdb layout", func() {
			ctx := testspecs.SetupTenantContext("test-tenant")
			ctx = graph.ExportContextWithIdentity(ctx, &authn.Identity{
				Subject:  "admin@test.com",
				TenantID: "test-tenant",
				Roles:    []string{authn.RoleAdmin},
			})

			var capturedParams map[string]string
			mockGraph.executeQueryFunc = func(ctx context.Context, tenant, cypher string, params map[string]string) ([]graphdb.QueryRow, error) {
				capturedParams = params
				return nil, nil
			}

			asOf := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
			resp, err := svc.TemporalQuery(ctx, connect.NewRequest(&pb.TemporalQueryRequest{
				TenantContext: &pb.TenantContext{TenantId: "test-tenant"},
				Cypher:        "MATCH (n) WHERE n.valid_from <= $as_of RETURN n",
				AsOf:          timestamppb.New(asOf),
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			// AGE compares temporal properties as strings, so the as_of parameter
			// must use the same fixed-width layout stored properties use
			// (graphdb.FormatTime), not RFC3339Nano which trims trailing zeros
			// and would sort a whole-second as_of after a sub-second property value.
			Expect(capturedParams["as_of"]).To(Equal(graphdb.FormatTime(asOf)))
			Expect(capturedParams["as_of"]).To(Equal("2025-01-01T12:00:00.000000000Z"))
		})
	})
})
