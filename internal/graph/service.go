package graph

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"
	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Service implements GraphServiceHandler and gateway.GraphBackend.
type Service struct {
	graph     graphdb.GraphDB
	vectors   vectordb.VectorDB
	bus       natsbus.Client
	resolvers *ResolverRegistry
	logger    *slog.Logger
	tracer    trace.Tracer

	// maxBulkEdges bounds BulkCreateEdges requests at the RPC boundary so
	// graph drivers never see an unbounded batch.
	maxBulkEdges int

	// metrics
	rpcCounter         metric.Int64Counter
	rpcLatency         metric.Float64Histogram
	eventCounter       metric.Int64Counter
	materializeLatency metric.Float64Histogram

	// subscriber lifecycle (used in Task 4)
	mu  sync.Mutex
	sub natsbus.Subscription
}

// New creates a Graph Service.
func New(graph graphdb.GraphDB, vectors vectordb.VectorDB, bus natsbus.Client, opts ...Option) *Service {
	s := &Service{
		graph:        graph,
		vectors:      vectors,
		bus:          bus,
		resolvers:    NewResolverRegistry(),
		logger:       slog.Default(),
		maxBulkEdges: config.DefaultGraphMaxBulkEdges,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// startSpan begins a trace span, nil-safe.
// Used in Tasks 3-7 for gRPC handler telemetry.
func (s *Service) startSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	return telemetry.StartSpan(s.tracer, ctx, name)
}

// recordRPC records RPC metrics, nil-safe.
// Used in Tasks 3-7 for RPC observability.
func (s *Service) recordRPC(ctx context.Context, method string, start time.Time, code connect.Code) {
	if s.rpcCounter != nil {
		s.rpcCounter.Add(ctx, 1,
			metric.WithAttributes(
				attribute.String("method", method),
				attribute.String("status", code.String()),
			))
	}
	if s.rpcLatency != nil {
		s.rpcLatency.Record(ctx, float64(time.Since(start).Milliseconds()),
			metric.WithAttributes(attribute.String("method", method)))
	}
}

// rpcError turns a graph or vector store error into the Connect error an RPC
// returns. An error mapped to a specific code (not found, invalid argument,
// ...) is the client's to fix, so it keeps its message, reworded by
// graphErrorMessage. Anything mapped to CodeInternal is driver or database
// detail (SQL and Cypher fragments, SQLSTATE codes, internal graph names) that
// must not reach a client (CWE-209): the client gets a fixed message pointing
// at the trace, and the full error is logged here. This is the one place the
// service logs an error it also returns, because once the detail is withheld
// from the caller the log is the only record of it. The logged error may
// echo fragments of client-supplied Cypher; that stays server-side, and no
// credentials reach driver errors. trace_id is set
// explicitly because no binary installs telemetry.Init's trace-correlating
// slog handler yet.
func (s *Service) rpcError(ctx context.Context, method string, code connect.Code, err error) *connect.Error {
	if code != connect.CodeInternal {
		return connect.NewError(code, errors.New(graphErrorMessage(err)))
	}
	traceID := telemetry.TraceIDFromContext(ctx)
	s.logger.ErrorContext(ctx, "graph RPC failed", "method", method, "trace_id", traceID, "error", err)
	if traceID == "" {
		return connect.NewError(code, fmt.Errorf(
			"internal graph error; the server logged the cause. Retry the request, and if it keeps failing, give an operator the time of the failure and the method name (%s)", method))
	}
	return connect.NewError(code, fmt.Errorf(
		"internal graph error; the server logged the cause under trace %s. Retry the request, and if it keeps failing, give that trace ID to an operator", traceID))
}

// extractTenant validates the request's tenant context against the
// context-propagated tenant. Returns the validated tenant ID or a Connect error.
// Used in Tasks 3-7 for tenant validation in RPC handlers.
func (s *Service) extractTenant(ctx context.Context, tc *pb.TenantContext) (string, error) {
	if tc == nil || tc.GetTenantId() == "" {
		return "", connect.NewError(connect.CodeInvalidArgument, errors.New("tenant_context is required"))
	}
	ctxTenant, err := tenant.FromContext(ctx)
	if err != nil {
		return "", connect.NewError(connect.CodeUnauthenticated, errors.New("no tenant in context"))
	}
	if ctxTenant != tc.GetTenantId() {
		return "", connect.NewError(connect.CodePermissionDenied, errors.New("tenant mismatch"))
	}
	return ctxTenant, nil
}
