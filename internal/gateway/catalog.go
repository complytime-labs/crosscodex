package gateway

import (
	"context"
	"time"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Service) ListCatalogs(ctx context.Context, req *pb.ListCatalogsRequest) (*pb.ListCatalogsResponse, error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, status.Error(codes.Unauthenticated, "not authenticated")
	}

	ctx, endSpan := s.startHandlerSpan(ctx, "ListCatalogs", identity)
	defer endSpan()

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

	ctx, endSpan := s.startHandlerSpan(ctx, "GetCatalog", identity,
		attribute.String("catalog.id", req.GetCatalogId()))
	defer endSpan()

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

	ctx, endSpan := s.startHandlerSpan(ctx, "SearchControls", identity)
	defer endSpan()

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

	ctx, endSpan := s.startHandlerSpan(ctx, "GetControl", identity,
		attribute.String("control.id", req.GetControlId()))
	defer endSpan()

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
