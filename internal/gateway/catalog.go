package gateway

import (
	"context"
	"time"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Service) ListCatalogs(ctx context.Context, req *pb.ListCatalogsRequest) (*pb.ListCatalogsResponse, error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, status.Error(codes.Unauthenticated, "not authenticated")
	}

	if s.tracer != nil {
		var span trace.Span
		ctx, span = s.tracer.Start(ctx, "gateway.ListCatalogs",
			trace.WithAttributes(
				attribute.String("rpc.method", "ListCatalogs"),
				attribute.String("tenant.id", identity.TenantID),
				attribute.String("user.id", identity.Subject),
			))
		defer span.End()
	}

	if s.catalog == nil {
		return nil, status.Error(codes.Unavailable, "catalog backend not configured")
	}

	tc := buildTenantContext(identity)
	req.TenantContext = tc

	resp, err := s.catalog.ListCatalogs(ctx, req)
	if err != nil {
		s.recordMetrics(ctx, "ListCatalogs", start, status.Code(err))
		return nil, err
	}

	s.recordMetrics(ctx, "ListCatalogs", start, codes.OK)
	return resp, nil
}

func (s *Service) GetCatalog(ctx context.Context, req *pb.GetCatalogRequest) (*pb.GetCatalogResponse, error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, status.Error(codes.Unauthenticated, "not authenticated")
	}

	if s.tracer != nil {
		var span trace.Span
		ctx, span = s.tracer.Start(ctx, "gateway.GetCatalog",
			trace.WithAttributes(
				attribute.String("rpc.method", "GetCatalog"),
				attribute.String("tenant.id", identity.TenantID),
				attribute.String("user.id", identity.Subject),
				attribute.String("catalog.id", req.GetCatalogId()),
			))
		defer span.End()
	}

	if req.GetCatalogId() == "" {
		return nil, status.Error(codes.InvalidArgument, "catalog_id is required")
	}

	if s.catalog == nil {
		return nil, status.Error(codes.Unavailable, "catalog backend not configured")
	}

	tc := buildTenantContext(identity)
	req.TenantContext = tc

	resp, err := s.catalog.GetCatalog(ctx, req)
	if err != nil {
		s.recordMetrics(ctx, "GetCatalog", start, status.Code(err))
		return nil, err
	}

	s.recordMetrics(ctx, "GetCatalog", start, codes.OK)
	return resp, nil
}

func (s *Service) SearchControls(ctx context.Context, req *pb.SearchControlsRequest) (*pb.SearchControlsResponse, error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, status.Error(codes.Unauthenticated, "not authenticated")
	}

	if s.tracer != nil {
		var span trace.Span
		ctx, span = s.tracer.Start(ctx, "gateway.SearchControls",
			trace.WithAttributes(
				attribute.String("rpc.method", "SearchControls"),
				attribute.String("tenant.id", identity.TenantID),
				attribute.String("user.id", identity.Subject),
			))
		defer span.End()
	}

	if req.GetQuery() == "" {
		return nil, status.Error(codes.InvalidArgument, "query is required")
	}

	if s.catalog == nil {
		return nil, status.Error(codes.Unavailable, "catalog backend not configured")
	}

	tc := buildTenantContext(identity)
	req.TenantContext = tc

	resp, err := s.catalog.SearchControls(ctx, req)
	if err != nil {
		s.recordMetrics(ctx, "SearchControls", start, status.Code(err))
		return nil, err
	}

	s.recordMetrics(ctx, "SearchControls", start, codes.OK)
	return resp, nil
}

func (s *Service) GetControl(ctx context.Context, req *pb.GetControlRequest) (*pb.GetControlResponse, error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, status.Error(codes.Unauthenticated, "not authenticated")
	}

	if s.tracer != nil {
		var span trace.Span
		ctx, span = s.tracer.Start(ctx, "gateway.GetControl",
			trace.WithAttributes(
				attribute.String("rpc.method", "GetControl"),
				attribute.String("tenant.id", identity.TenantID),
				attribute.String("user.id", identity.Subject),
				attribute.String("control.id", req.GetControlId()),
			))
		defer span.End()
	}

	if req.GetControlId() == "" {
		return nil, status.Error(codes.InvalidArgument, "control_id is required")
	}

	if s.catalog == nil {
		return nil, status.Error(codes.Unavailable, "catalog backend not configured")
	}

	tc := buildTenantContext(identity)
	req.TenantContext = tc

	resp, err := s.catalog.GetControl(ctx, req)
	if err != nil {
		s.recordMetrics(ctx, "GetControl", start, status.Code(err))
		return nil, err
	}

	s.recordMetrics(ctx, "GetControl", start, codes.OK)
	return resp, nil
}
