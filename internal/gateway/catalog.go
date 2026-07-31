package gateway

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"go.opentelemetry.io/otel/attribute"
)

func (s *Service) ListCatalogs(ctx context.Context, req *connect.Request[pb.ListCatalogsRequest]) (*connect.Response[pb.ListCatalogsResponse], error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}

	ctx, endSpan := s.startHandlerSpan(ctx, "ListCatalogs", identity)
	defer endSpan()

	if s.catalog == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("catalog backend not configured"))
	}

	tc := buildTenantContext(identity)
	req.Msg.TenantContext = tc

	resp, err := s.catalog.ListCatalogs(ctx, req)
	if err != nil {
		s.recordMetrics(ctx, "ListCatalogs", start, connect.CodeOf(err))
		return nil, err
	}

	s.recordMetrics(ctx, "ListCatalogs", start, connect.Code(0))
	return resp, nil
}

func (s *Service) GetCatalog(ctx context.Context, req *connect.Request[pb.GetCatalogRequest]) (*connect.Response[pb.GetCatalogResponse], error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}

	ctx, endSpan := s.startHandlerSpan(ctx, "GetCatalog", identity,
		attribute.String("catalog.id", req.Msg.GetCatalogId()))
	defer endSpan()

	if req.Msg.GetCatalogId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("catalog_id is required"))
	}

	if s.catalog == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("catalog backend not configured"))
	}

	tc := buildTenantContext(identity)
	req.Msg.TenantContext = tc

	resp, err := s.catalog.GetCatalog(ctx, req)
	if err != nil {
		s.recordMetrics(ctx, "GetCatalog", start, connect.CodeOf(err))
		return nil, err
	}

	s.recordMetrics(ctx, "GetCatalog", start, connect.Code(0))
	return resp, nil
}

func (s *Service) SearchControls(ctx context.Context, req *connect.Request[pb.SearchControlsRequest]) (*connect.Response[pb.SearchControlsResponse], error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}

	ctx, endSpan := s.startHandlerSpan(ctx, "SearchControls", identity)
	defer endSpan()

	if req.Msg.GetQuery() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("query is required"))
	}

	if s.catalog == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("catalog backend not configured"))
	}

	tc := buildTenantContext(identity)
	req.Msg.TenantContext = tc

	resp, err := s.catalog.SearchControls(ctx, req)
	if err != nil {
		s.recordMetrics(ctx, "SearchControls", start, connect.CodeOf(err))
		return nil, err
	}

	s.recordMetrics(ctx, "SearchControls", start, connect.Code(0))
	return resp, nil
}

func (s *Service) GetControl(ctx context.Context, req *connect.Request[pb.GetControlRequest]) (*connect.Response[pb.GetControlResponse], error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}

	ctx, endSpan := s.startHandlerSpan(ctx, "GetControl", identity,
		attribute.String("control.id", req.Msg.GetControlId()))
	defer endSpan()

	if req.Msg.GetControlId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("control_id is required"))
	}

	if s.catalog == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("catalog backend not configured"))
	}

	tc := buildTenantContext(identity)
	req.Msg.TenantContext = tc

	resp, err := s.catalog.GetControl(ctx, req)
	if err != nil {
		s.recordMetrics(ctx, "GetControl", start, connect.CodeOf(err))
		return nil, err
	}

	s.recordMetrics(ctx, "GetControl", start, connect.Code(0))
	return resp, nil
}
