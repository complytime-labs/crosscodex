package graph

import (
	"context"
	"log/slog"
	"sync"
	"time"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Service implements GraphServiceServer and gateway.GraphBackend.
type Service struct {
	pb.UnimplementedGraphServiceServer

	graph     graphdb.GraphDB
	vectors   vectordb.VectorDB
	bus       natsbus.Client
	resolvers *ResolverRegistry
	logger    *slog.Logger
	tracer    trace.Tracer

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
		graph:     graph,
		vectors:   vectors,
		bus:       bus,
		resolvers: NewResolverRegistry(),
		logger:    slog.Default(),
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
func (s *Service) recordRPC(ctx context.Context, method string, start time.Time, code codes.Code) {
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

// extractTenant validates the request's tenant context against the
// context-propagated tenant. Returns the validated tenant ID or a gRPC error.
// Used in Tasks 3-7 for tenant validation in RPC handlers.
func (s *Service) extractTenant(ctx context.Context, tc *pb.TenantContext) (string, error) {
	if tc == nil || tc.GetTenantId() == "" {
		return "", status.Error(codes.InvalidArgument, "tenant_context is required")
	}
	ctxTenant, err := tenant.FromContext(ctx)
	if err != nil {
		return "", status.Error(codes.Unauthenticated, "no tenant in context")
	}
	if ctxTenant != tc.GetTenantId() {
		return "", status.Error(codes.PermissionDenied, "tenant mismatch")
	}
	return ctxTenant, nil
}
