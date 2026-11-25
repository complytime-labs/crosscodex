package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// ListCatalogControls serves a catalog's controls as composeControls gives
// them to analysis, through the readers WithCatalogNameReader and
// WithCatalogControlsReader wire. See ServeCatalogControls.
func (s *Service) ListCatalogControls(ctx context.Context, req *connect.Request[pb.ListCatalogControlsRequest]) (*connect.Response[pb.ListCatalogControlsResponse], error) {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.ListCatalogControls")
	defer span.End()
	span.SetAttributes(attribute.String("catalog.id", req.Msg.GetCatalogId()))
	if tenantID, err := tenant.FromContext(ctx); err == nil {
		span.SetAttributes(attribute.String("tenant.id", tenantID))
	}

	resp, err := ServeCatalogControls(ctx, s.catalogNames, s.catalogControls, s.logger, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, err
	}
	span.SetAttributes(attribute.Int("controls.count", len(resp.Msg.GetControls())))
	span.SetStatus(otelcodes.Ok, "")
	return resp, nil
}

// ServeCatalogControls answers a ListCatalogControls request. It is the one
// implementation behind Service.ListCatalogControls and the CLI's embedded
// mode, which has no Service, so both return the same composed controls and
// the same errors.
//
// The tenant is ctx's; a request without tenant_context, or whose
// tenant_context names another tenant, is refused without naming either. The
// catalog must be one of the tenant's (names decides, under RLS): an unknown
// catalog and another tenant's get one fixed NotFound, built here, so the
// answer is no existence oracle. Existence is decided by the catalog row, not
// by the control count, so a catalog of the tenant's without controls
// returns an empty list. Without either reader nothing can be confirmed or
// read, so the request is refused (fail closed). Store errors are logged
// with logger, which must be non-nil, and withheld from the caller (CWE-209).
func ServeCatalogControls(ctx context.Context, names CatalogNameReader, controls CatalogControlsReader, logger *slog.Logger,
	req *connect.Request[pb.ListCatalogControlsRequest]) (*connect.Response[pb.ListCatalogControlsResponse], error) {
	tenantID, err := requestTenant(ctx, req.Msg.GetTenantContext())
	if err != nil {
		return nil, err
	}
	catalogID := req.Msg.GetCatalogId()
	if catalogID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(
			`catalog_id is required: name the imported catalog whose controls to list. `+
				`Run "crosscodex catalog list" to find its ID`))
	}
	if names == nil || controls == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New(
			"cannot list the catalog's controls: the pipeline has no catalog reader configured, so it can neither confirm "+
				"that the catalog is your tenant's nor read its controls. An operator must wire the catalog name and "+
				"controls readers (pipeline.WithCatalogNameReader, pipeline.WithCatalogControlsReader)"))
	}

	unreadable := connect.NewError(connect.CodeUnavailable, fmt.Errorf(
		"cannot list the controls of catalog %q: the catalog store could not be read. "+
			"Retry the request; if it keeps failing, ask an operator to check the database", catalogID))
	_, composed, err := tenantCatalogControls(ctx, names, controls, tenantID, catalogID)
	if errors.Is(err, ErrCatalogNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf(
			`catalog %q is not a catalog of your tenant: no imported catalog has that ID. `+
				`Run "crosscodex catalog list" to find the ID of an imported catalog`, catalogID))
	}
	if err != nil {
		logger.ErrorContext(ctx, "catalog read failed", "operation", "ListCatalogControls",
			"tenant", tenantID, "catalog_id", catalogID, "error", err)
		return nil, unreadable
	}
	return connect.NewResponse(&pb.ListCatalogControlsResponse{Controls: composed}), nil
}

// requestTenant returns the tenant of ctx, which a tenant-scoped request's
// tenant_context must name: a request without one (or a ctx without a
// tenant) is InvalidArgument, and one naming another tenant is
// PermissionDenied, without naming either tenant.
func requestTenant(ctx context.Context, tc *pb.TenantContext) (string, error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil || tc == nil {
		return "", connect.NewError(connect.CodeInvalidArgument, errors.New("missing tenant context"))
	}
	if tc.GetTenantId() != tenantID {
		return "", connect.NewError(connect.CodePermissionDenied, errors.New("tenant_context does not match the authenticated tenant"))
	}
	return tenantID, nil
}

// tenantCatalogControls reads catalogID of tenantID: its snapshot (names
// decides that the catalog is the tenant's, under RLS) and then its
// composed controls. A catalog that is not the tenant's, unknown or
// another tenant's alike, returns an error wrapping ErrCatalogNotFound;
// any other error is a store error, for the caller to log and withhold.
func tenantCatalogControls(ctx context.Context, names CatalogNameReader, controls CatalogControlsReader,
	tenantID, catalogID string) (*pb.CatalogSnapshot, []*pb.Control, error) {
	snapshot, err := names.CatalogSnapshot(ctx, tenantID, catalogID)
	if err != nil {
		return nil, nil, fmt.Errorf("looking up catalog %s: %w", catalogID, err)
	}
	composed, err := controls.Controls(ctx, tenantID, catalogID)
	if err != nil {
		return nil, nil, fmt.Errorf("reading the controls of catalog %s: %w", catalogID, err)
	}
	return snapshot, composed, nil
}
