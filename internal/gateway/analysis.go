package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"go.opentelemetry.io/otel/attribute"
)

// StartAnalysis queues a FULL_ANALYSIS job over a catalog the caller's
// tenant already imported. Upload never starts analysis; this RPC is the
// only way to.
func (s *Service) StartAnalysis(ctx context.Context, req *connect.Request[pb.StartAnalysisRequest]) (*connect.Response[pb.StartAnalysisResponse], error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}

	catalogID := req.Msg.GetSourceCatalogId()
	ctx, endSpan := s.startHandlerSpan(ctx, "StartAnalysis", identity,
		attribute.String("catalog.id", catalogID))
	defer endSpan()

	if catalogID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(
			"source_catalog_id is required. List catalogs to find a valid catalog_id"))
	}

	if s.catalog == nil || s.pipeline == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("required backends not configured"))
	}

	tc := buildTenantContext(identity)

	// The tenant-scoped lookup refuses an unknown or other-tenant catalog
	// with NotFound before any job row exists.
	if _, err := s.catalog.GetCatalog(ctx, connect.NewRequest(&pb.GetCatalogRequest{
		TenantContext: tc,
		CatalogId:     catalogID,
	})); err != nil {
		err = s.publicError(ctx, "StartAnalysis", err)
		s.recordMetrics(ctx, "StartAnalysis", start, connect.CodeOf(err))
		return nil, err
	}

	jobResp, err := s.pipeline.CreateJob(ctx, connect.NewRequest(&pb.CreateJobRequest{
		TenantContext: tc,
		JobType:       pb.JobType_JOB_TYPE_FULL_ANALYSIS,
		Config: &pb.JobConfig{
			CatalogId:       catalogID,
			SynthesisConfig: req.Msg.GetSynthesisConfig(),
		},
	}))
	if err != nil {
		err = s.publicError(ctx, "StartAnalysis", err)
		s.recordMetrics(ctx, "StartAnalysis", start, connect.CodeOf(err))
		return nil, err
	}

	jobID := jobResp.Msg.GetJobId()
	if jobID == "" {
		s.recordMetrics(ctx, "StartAnalysis", start, connect.CodeInternal)
		return nil, connect.NewError(connect.CodeInternal, errors.New("pipeline backend returned empty job_id"))
	}

	s.emitAttestation(ctx, "gateway.StartAnalysis",
		[]attestation.Artifact{{URI: fmt.Sprintf("catalog://%s/%s", identity.TenantID, catalogID)}},
		[]attestation.Artifact{{URI: fmt.Sprintf("job://%s/%s", identity.TenantID, jobID)}},
		nil)

	s.recordMetrics(ctx, "StartAnalysis", start, connect.Code(0))
	return connect.NewResponse(&pb.StartAnalysisResponse{
		JobId:  jobID,
		Status: jobResp.Msg.GetStatus(),
	}), nil
}
