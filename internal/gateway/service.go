package gateway

import (
	"context"
	"log/slog"
	"time"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
)

var _ pb.GatewayServiceServer = (*Service)(nil)

type Service struct {
	pb.UnimplementedGatewayServiceServer

	authn     *authn.Registry
	ingestion IngestionBackend
	catalog   CatalogBackend
	pipeline  PipelineBackend
	graph     GraphBackend
	feedback  FeedbackBackend
	admin     AdminBackend
	attestor  attestation.Generator
	tracer    trace.Tracer
	meter     metric.Meter
	logger    *slog.Logger

	requestsTotal   metric.Int64Counter
	requestDuration metric.Int64Histogram
	authFailures    metric.Int64Counter
}

type ServiceOption func(*Service)

func NewService(opts ...ServiceOption) *Service {
	s := &Service{
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(s)
	}

	if s.meter != nil {
		var err error
		s.requestsTotal, err = s.meter.Int64Counter("crosscodex.gateway.requests.total")
		if err != nil {
			s.logger.Warn("failed to create requests counter", "error", err)
		}

		s.requestDuration, err = s.meter.Int64Histogram("crosscodex.gateway.request.duration_ms")
		if err != nil {
			s.logger.Warn("failed to create request duration histogram", "error", err)
		}

		s.authFailures, err = s.meter.Int64Counter("crosscodex.gateway.auth.failures.total")
		if err != nil {
			s.logger.Warn("failed to create auth failures counter", "error", err)
		}
	}

	return s
}

func WithAuthn(r *authn.Registry) ServiceOption {
	return func(s *Service) { s.authn = r }
}

func WithIngestionBackend(b IngestionBackend) ServiceOption {
	return func(s *Service) { s.ingestion = b }
}

func WithCatalogBackend(b CatalogBackend) ServiceOption {
	return func(s *Service) { s.catalog = b }
}

func WithPipelineBackend(b PipelineBackend) ServiceOption {
	return func(s *Service) { s.pipeline = b }
}

func WithGraphBackend(b GraphBackend) ServiceOption {
	return func(s *Service) { s.graph = b }
}

func WithFeedbackBackend(b FeedbackBackend) ServiceOption {
	return func(s *Service) { s.feedback = b }
}

func WithAdminBackend(b AdminBackend) ServiceOption {
	return func(s *Service) { s.admin = b }
}

func WithAttestor(a attestation.Generator) ServiceOption {
	return func(s *Service) { s.attestor = a }
}

func WithTelemetry(t trace.Tracer, m metric.Meter) ServiceOption {
	return func(s *Service) {
		s.tracer = t
		s.meter = m
	}
}

func WithLogger(l *slog.Logger) ServiceOption {
	return func(s *Service) { s.logger = l }
}

func buildTenantContext(identity *authn.Identity) *pb.TenantContext {
	return &pb.TenantContext{
		TenantId: identity.TenantID,
	}
}

func (s *Service) recordMetrics(ctx context.Context, method string, start time.Time, code codes.Code) {
	if s.requestsTotal != nil {
		s.requestsTotal.Add(ctx, 1,
			metric.WithAttributes(
				attribute.String("method", method),
				attribute.String("status", code.String()),
			))
	}
	if s.requestDuration != nil {
		s.requestDuration.Record(ctx, time.Since(start).Milliseconds(),
			metric.WithAttributes(attribute.String("method", method)))
	}
}
