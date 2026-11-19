package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// remoteStageRecord returns the graph role's cleaning.CurrentFunc: it reads a
// stage's current prompt and models from the pipeline role, which owns the
// prompt registry and model configuration, through client.
//
// Every failure is returned, never reported as "not found": an unknown record
// would let the engine judge verdicts against nothing, so a pass fails closed
// when the pipeline role can't answer.
//
// An RPC failure is logged here with its cause and returned as a fresh
// CodeUnavailable error naming pipeline.endpoint, never wrapping the
// pipeline's error. The engine and the cleaning handler pass it through, and
// connect-go sends the first *connect.Error in the chain: a wrapped pipeline
// error would reach the API caller as its own PermissionDenied or NotFound,
// or with a dial error naming an internal address (CWE-209).
func remoteStageRecord(client crosscodexv1connect.PipelineServiceClient) cleaning.CurrentFunc {
	return func(ctx context.Context, tenantID, stage string) (cleaning.StageRecord, bool, error) {
		ctx, err := tenant.WithTenant(ctx, tenantID)
		if err != nil {
			return cleaning.StageRecord{}, false, fmt.Errorf("current stage record: %w", err)
		}
		resp, err := client.GetCurrentStageRecord(ctx, connect.NewRequest(&pb.GetCurrentStageRecordRequest{
			TenantContext: &pb.TenantContext{TenantId: tenantID},
			Stage:         stage,
		}))
		if err != nil {
			slog.ErrorContext(ctx, "current stage record via pipeline.endpoint failed", "tenant", tenantID, "stage", stage, "error", err)
			return cleaning.StageRecord{}, false, connect.NewError(connect.CodeUnavailable, errors.New(
				"current stage record via pipeline.endpoint: the pipeline role did not answer, so the cleaning pass was refused. "+
					"Check that pipeline.endpoint reaches the pipeline role and that the pipeline role's tls.service_identities "+
					"lists this graph role's client certificate; the graph role's log has the cause"))
		}
		if !resp.Msg.GetFound() {
			return cleaning.StageRecord{}, false, nil
		}
		return cleaning.StageRecord{
			PromptName:    resp.Msg.GetPromptName(),
			PromptVersion: resp.Msg.GetPromptVersion(),
			Models:        resp.Msg.GetModels(),
		}, true, nil
	}
}
