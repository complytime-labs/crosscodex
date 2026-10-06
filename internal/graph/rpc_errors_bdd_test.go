package graph_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
)

var _ = Describe("RPC error messages", func() {
	const tenantID = "test-tenant"
	// driverDetail stands in for what a driver returns from PostgreSQL: SQL,
	// a SQLSTATE and an internal graph name, none of which may reach a client.
	const driverDetail = `ERROR: syntax error at or near "MATCH" (SQLSTATE 42601) in graph crosscodex_test_tenant`
	traceID := trace.TraceID{0x0a, 0xf7, 0x65, 0x19, 0x16, 0xcd, 0x43, 0xdd, 0x84, 0x48, 0xeb, 0x21, 0x1c, 0x80, 0x31, 0x9c}

	var (
		logs *bytes.Buffer
		svc  *graph.Service
	)

	// rpcs calls every graph RPC that forwards a driver or vector store
	// error, with a fake that fails every call with err.
	rpcs := func(err error) map[string]func(context.Context) error {
		mg := &mockGraphDB{
			getNodeFunc:  func(context.Context, string, string) (*graphdb.Node, error) { return nil, err },
			getEdgeFunc:  func(context.Context, string, string) (*graphdb.EdgeWithEndpoints, error) { return nil, err },
			traverseFunc: func(context.Context, string, graphdb.TraversalQuery) ([]graphdb.Path, error) { return nil, err },
			executeQueryFunc: func(context.Context, string, string, map[string]string) ([]graphdb.QueryRow, error) {
				return nil, err
			},
			createNodeFunc: func(context.Context, string, graphdb.Node) error { return err },
			createEdgeFunc: func(context.Context, string, string, string, graphdb.Edge) error { return err },
			bulkCreateEdgesFunc: func(context.Context, string, []graphdb.BulkEdge) ([]string, error) {
				return nil, err
			},
			supersedeFactFunc: func(context.Context, string, graphdb.SupersedeRequest) (bool, error) { return false, err },
		}
		mv := &mockVectorDB{findSimilarFunc: func(context.Context, string, vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
			return nil, err
		}}
		logs = &bytes.Buffer{}
		svc = graph.New(mg, mv, nil, graph.WithLogger(slog.New(slog.NewTextHandler(logs, nil))))
		tc := &pb.TenantContext{TenantId: tenantID}
		edge := &pb.CreateEdgeRequest{SourceNodeId: "a", TargetNodeId: "b", Label: "MAPS"}
		return map[string]func(context.Context) error{
			"GetNode": func(ctx context.Context) error {
				_, e := svc.GetNode(ctx, connect.NewRequest(&pb.GetNodeRequest{TenantContext: tc, NodeId: "a"}))
				return e
			},
			"GetEdge": func(ctx context.Context) error {
				_, e := svc.GetEdge(ctx, connect.NewRequest(&pb.GetEdgeRequest{TenantContext: tc, EdgeId: "e"}))
				return e
			},
			"Traverse": func(ctx context.Context) error {
				_, e := svc.Traverse(ctx, connect.NewRequest(&pb.TraverseRequest{TenantContext: tc, StartNodeId: "a", MaxDepth: 1}))
				return e
			},
			"Query": func(ctx context.Context) error {
				_, e := svc.Query(ctx, connect.NewRequest(&pb.QueryRequest{TenantContext: tc, Cypher: "MATCH (n) RETURN n"}))
				return e
			},
			"TemporalQuery": func(ctx context.Context) error {
				_, e := svc.TemporalQuery(ctx, connect.NewRequest(&pb.TemporalQueryRequest{TenantContext: tc, Cypher: "MATCH (n) RETURN n", AsOf: timestamppb.Now()}))
				return e
			},
			"SimilaritySearch": func(ctx context.Context) error {
				_, e := svc.SimilaritySearch(ctx, connect.NewRequest(&pb.SimilaritySearchRequest{
					TenantContext: tc,
					Query:         &pb.SimilaritySearchRequest_QueryEmbedding{QueryEmbedding: &pb.EmbeddingQuery{Embeddings: []float32{0.1}}},
				}))
				return e
			},
			"CreateNode": func(ctx context.Context) error {
				_, e := svc.CreateNode(ctx, connect.NewRequest(&pb.CreateNodeRequest{TenantContext: tc, Label: "Control"}))
				return e
			},
			"CreateEdge": func(ctx context.Context) error {
				_, e := svc.CreateEdge(ctx, connect.NewRequest(&pb.CreateEdgeRequest{TenantContext: tc, SourceNodeId: "a", TargetNodeId: "b", Label: "MAPS"}))
				return e
			},
			"BulkCreateEdges": func(ctx context.Context) error {
				_, e := svc.BulkCreateEdges(ctx, connect.NewRequest(&pb.BulkCreateEdgesRequest{TenantContext: tc, Edges: []*pb.CreateEdgeRequest{edge}}))
				return e
			},
			"SupersedeFact": func(ctx context.Context) error {
				_, e := svc.SupersedeFact(ctx, connect.NewRequest(&pb.SupersedeFactRequest{
					TenantContext: tc, Target: &pb.SupersedeFactRequest_NodeId{NodeId: "a"}, SupersededAt: timestamppb.Now(),
				}))
				return e
			},
		}
	}

	// rpcCtx is a tenant- and admin-scoped context. withTrace adds a span
	// context, as the Connect OpenTelemetry interceptor would.
	rpcCtx := func(withTrace bool) context.Context {
		ctx := testspecs.SetupTenantContext(tenantID)
		ctx = graph.ExportContextWithIdentity(ctx, &authn.Identity{Subject: "admin@test.com", TenantID: tenantID, Roles: []string{authn.RoleAdmin}})
		if withTrace {
			ctx = trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
				TraceID: traceID, SpanID: trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8}, TraceFlags: trace.FlagsSampled,
			}))
		}
		return ctx
	}

	message := func(err error) string {
		var ce *connect.Error
		Expect(errors.As(err, &ce)).To(BeTrue(), "expected a *connect.Error, got %v", err)
		return ce.Message()
	}

	methods := []string{"GetNode", "GetEdge", "Traverse", "Query", "TemporalQuery", "SimilaritySearch", "CreateNode", "CreateEdge", "BulkCreateEdges", "SupersedeFact"}

	It("hides an unclassified error behind the trace ID and logs it in full, for every RPC", func() {
		calls := rpcs(errors.New(driverDetail))
		Expect(calls).To(HaveLen(len(methods)))
		for _, m := range methods {
			err := calls[m](rpcCtx(true))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal), m)
			Expect(message(err)).To(Equal(fmt.Sprintf(
				"internal graph error; the server logged the cause under trace %s. Retry the request, and if it keeps failing, give that trace ID to an operator", traceID)), m)
			Expect(message(err)).NotTo(ContainSubstring("SQLSTATE"), m)
		}
		for _, m := range methods {
			Expect(logs.String()).To(ContainSubstring("method="+m), "the full error must be logged for %s", m)
		}
		Expect(logs.String()).To(ContainSubstring("level=ERROR"))
		Expect(logs.String()).To(ContainSubstring("SQLSTATE 42601"))
		Expect(logs.String()).To(ContainSubstring("trace_id=" + traceID.String()))
	})

	It("names the method when there is no trace to point at", func() {
		calls := rpcs(errors.New(driverDetail))
		for _, m := range methods {
			err := calls[m](rpcCtx(false))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal), m)
			Expect(message(err)).To(Equal(fmt.Sprintf(
				"internal graph error; the server logged the cause. Retry the request, and if it keeps failing, give an operator the time of the failure and the method name (%s)", m)), m)
		}
	})

	DescribeTable("keeps the message of an error the client can act on, and does not log it",
		func(err error, code connect.Code) {
			for _, m := range []string{"GetNode", "GetEdge", "Traverse", "Query", "TemporalQuery", "CreateNode", "CreateEdge", "BulkCreateEdges", "SupersedeFact"} {
				got := rpcs(err)[m](rpcCtx(true))
				Expect(connect.CodeOf(got)).To(Equal(code), m)
				Expect(message(got)).To(Equal(err.Error()), m)
				Expect(logs.String()).To(BeEmpty(), m)
			}
		},
		Entry("not found", fmt.Errorf("get node: %w", graphdb.ErrNodeNotFound), connect.CodeNotFound),
		Entry("invalid argument", fmt.Errorf("label %q: %w", "bad label", graphdb.ErrInvalidCypher), connect.CodeInvalidArgument),
		Entry("unsupported", fmt.Errorf("execute query: %w", graphdb.ErrNotSupported), connect.CodeUnimplemented),
	)

	DescribeTable("keeps the code of a classified error that wraps driver text, but replaces its message",
		func(sentinel error, code connect.Code, want string) {
			err := fmt.Errorf("execute query: %w: %w", sentinel, errors.New(driverDetail))
			for _, m := range []string{"GetNode", "GetEdge", "Traverse", "Query", "TemporalQuery", "CreateNode", "CreateEdge", "BulkCreateEdges", "SupersedeFact"} {
				got := rpcs(err)[m](rpcCtx(true))
				Expect(connect.CodeOf(got)).To(Equal(code), m)
				Expect(message(got)).To(Equal(want), m)
				Expect(message(got)).NotTo(ContainSubstring("SQLSTATE"), m)
				Expect(message(got)).NotTo(ContainSubstring("crosscodex_test_tenant"), m)
			}
		},
		Entry("graph not found", graphdb.ErrGraphNotFound, connect.CodeNotFound,
			"graph not found: the tenant's graph does not exist; create the tenant before using its graph"),
		Entry("read-only violation", graphdb.ErrReadOnlyViolation, connect.CodePermissionDenied,
			"read-only transaction violation: the query writes to the graph, but Query and TemporalQuery are read-only; write with CreateNode, CreateEdge, BulkCreateEdges or SupersedeFact instead"),
	)

	It("keeps the default logger when WithLogger is given nil, so an Internal error does not panic", func() {
		mg := &mockGraphDB{getNodeFunc: func(context.Context, string, string) (*graphdb.Node, error) {
			return nil, errors.New(driverDetail)
		}}
		nilLogged := graph.New(mg, &mockVectorDB{}, nil, graph.WithLogger(nil))
		var err error
		Expect(func() {
			_, err = nilLogged.GetNode(rpcCtx(true), connect.NewRequest(&pb.GetNodeRequest{TenantContext: &pb.TenantContext{TenantId: tenantID}, NodeId: "a"}))
		}).NotTo(Panic())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
	})

	It("keeps a classified vector store error's message", func() {
		err := fmt.Errorf("find similar: %w", vectordb.ErrInvalidDimension)
		got := rpcs(err)["SimilaritySearch"](rpcCtx(true))
		Expect(connect.CodeOf(got)).To(Equal(connect.CodeInvalidArgument))
		Expect(message(got)).To(Equal(err.Error()))
	})
})
