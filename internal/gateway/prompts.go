package gateway

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
)

// RenderPrompt renders an analyzer's prompt for one control pair. From
// catalogs alone any user of the tenant may render it, as any may read the
// catalogs (ListCatalogControls). With job_id it shows the job's stored
// classifications and verdicts, so the caller must own the job or be an
// admin, as for GetJob.
//
// Ownership is checked after the pipeline answers, not before: the pipeline
// gives an unknown job, another tenant's job and an unknown control one
// NotFound, and an earlier GetJob would answer an unknown job with its own.
// That NotFound is returned unchanged, to anyone. Every other answer about a
// job, a render or a refusal (which can tell of the job's catalogs, its
// verdicts or whether it ran), reaches only the owner or an admin; anyone
// else gets PermissionDenied. Rendering calls no LLM.
func (s *Service) RenderPrompt(ctx context.Context, req *connect.Request[pb.RenderPromptRequest]) (*connect.Response[pb.RenderPromptResponse], error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}

	ctx, endSpan := s.startHandlerSpan(ctx, "RenderPrompt", identity,
		attribute.String("analyzer", req.Msg.GetAnalyzer()),
		attribute.String("job.id", req.Msg.GetJobId()))
	defer endSpan()

	if s.pipeline == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("pipeline backend not configured"))
	}

	tc := buildTenantContext(identity)
	req.Msg.TenantContext = tc

	resp, err := s.pipeline.RenderPrompt(ctx, req)
	if jobID := req.Msg.GetJobId(); jobID != "" && connect.CodeOf(err) != connect.CodeNotFound {
		if authErr := s.authorizeJobOwner(ctx, "RenderPrompt", start, identity, tc, jobID); authErr != nil {
			return nil, authErr
		}
	}
	if err != nil {
		err = s.publicError(ctx, "RenderPrompt", err)
		s.recordMetrics(ctx, "RenderPrompt", start, connect.CodeOf(err))
		return nil, err
	}

	s.recordMetrics(ctx, "RenderPrompt", start, connect.Code(0))
	return resp, nil
}
