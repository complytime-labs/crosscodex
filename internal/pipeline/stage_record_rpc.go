package pipeline

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// GetCurrentStageRecord serves CurrentStageRecord to the graph role's cleaning
// pass. The tenant is the one the authenticated connection carries; a
// tenant_context naming another tenant is refused without naming either, so the
// answer reveals nothing about other tenants.
func (s *Service) GetCurrentStageRecord(ctx context.Context, req *connect.Request[pb.GetCurrentStageRecordRequest]) (*connect.Response[pb.GetCurrentStageRecordResponse], error) {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.GetCurrentStageRecord")
	defer span.End()

	tenantID, err := tenant.FromContext(ctx)
	if err != nil || req.Msg.GetTenantContext() == nil {
		if err == nil {
			err = errors.New("tenant_context is unset")
		}
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing tenant context"))
	}
	if req.Msg.GetTenantContext().GetTenantId() != tenantID {
		err := errors.New("tenant_context does not match the authenticated tenant")
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, connect.NewError(connect.CodePermissionDenied, err)
	}

	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.String("stage.name", req.Msg.GetStage()),
	)

	record, found, err := s.CurrentStageRecord(ctx, tenantID, req.Msg.GetStage())
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		// The cause can name prompt layers and configuration (CWE-209), so it
		// is logged here once and withheld from the caller.
		s.logger.ErrorContext(ctx, "get current stage record failed", "tenant", tenantID, "stage", req.Msg.GetStage(), "error", err)
		return nil, connect.NewError(connect.CodeInternal,
			errors.New("the current stage record could not be read. Check the pipeline role's logs for the cause"))
	}

	span.SetStatus(otelcodes.Ok, "")
	return connect.NewResponse(&pb.GetCurrentStageRecordResponse{
		Found:         found,
		PromptName:    record.PromptName,
		PromptVersion: record.PromptVersion,
		Models:        record.Models,
	}), nil
}
