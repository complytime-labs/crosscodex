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

// StartAnalysis queues a FULL_ANALYSIS job that maps the controls of a
// source catalog onto a target catalog, both imported by the caller's
// tenant. A target equal to the source analyses within one catalog. Upload
// never starts analysis; this RPC is the only way to.
func (s *Service) StartAnalysis(ctx context.Context, req *connect.Request[pb.StartAnalysisRequest]) (*connect.Response[pb.StartAnalysisResponse], error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}

	sourceID := req.Msg.GetSourceCatalogId()
	targetID := req.Msg.GetTargetCatalogId()
	bidirectional := req.Msg.GetBidirectional()
	ctx, endSpan := s.startHandlerSpan(ctx, "StartAnalysis", identity,
		attribute.String("catalog.id", sourceID),
		attribute.String("target_catalog.id", targetID),
		attribute.Bool("analysis.bidirectional", bidirectional))
	defer endSpan()

	if sourceID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(
			"source_catalog_id is required. List catalogs to find a valid catalog_id"))
	}
	if targetID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
			"target_catalog_id is required: an analysis maps the source catalog's controls onto a target catalog's. "+
				"Set it to the catalog to map onto (crosscodex run start --source %s --target <catalog-id>), "+
				"or to source_catalog_id to analyse within one catalog (crosscodex run start --source %s --target %s). "+
				"List catalogs to find a valid catalog_id", sourceID, sourceID, sourceID))
	}
	if bidirectional && targetID == sourceID {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(
			"bidirectional cannot be set when target_catalog_id equals source_catalog_id: an analysis within one catalog "+
				"already pairs its controls in both directions. Unset bidirectional, or set target_catalog_id to a different catalog"))
	}

	if s.catalog == nil || s.pipeline == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("required backends not configured"))
	}

	tc := buildTenantContext(identity)

	// The tenant-scoped lookups refuse an unknown or other-tenant catalog
	// with NotFound before any job row exists: the source first, then the
	// target unless it is the source.
	if _, err := s.catalog.GetCatalog(ctx, connect.NewRequest(&pb.GetCatalogRequest{
		TenantContext: tc,
		CatalogId:     sourceID,
	})); err != nil {
		err = s.publicError(ctx, "StartAnalysis", err)
		s.recordMetrics(ctx, "StartAnalysis", start, connect.CodeOf(err))
		return nil, err
	}
	if targetID != sourceID {
		if _, err := s.catalog.GetCatalog(ctx, connect.NewRequest(&pb.GetCatalogRequest{
			TenantContext: tc,
			CatalogId:     targetID,
		})); err != nil {
			// One fixed message for every NotFound, whatever the backend
			// said: a missing target and another tenant's target must read
			// the same, so the error is no existence oracle.
			if connect.CodeOf(err) == connect.CodeNotFound {
				// The caller gets the fixed message only; the backend's is
				// kept here, once, for an operator. Every field is the
				// caller's own: their tenant and the ID they sent.
				s.logger.DebugContext(ctx, "target catalog lookup returned NotFound; answering with the fixed message",
					"method", "StartAnalysis", "tenant", identity.TenantID, "target_catalog_id", targetID, "error", err)
				err = connect.NewError(connect.CodeNotFound, fmt.Errorf(
					"target catalog %q not found: your tenant has imported no catalog with that ID. "+
						"Run \"crosscodex catalog list\" to find the ID of an imported catalog, and pass it as target_catalog_id (--target)", targetID))
			}
			err = s.publicError(ctx, "StartAnalysis", err)
			s.recordMetrics(ctx, "StartAnalysis", start, connect.CodeOf(err))
			return nil, err
		}
	}

	jobResp, err := s.pipeline.CreateJob(ctx, connect.NewRequest(&pb.CreateJobRequest{
		TenantContext: tc,
		JobType:       pb.JobType_JOB_TYPE_FULL_ANALYSIS,
		Config: &pb.JobConfig{
			CatalogId:       sourceID,
			TargetCatalogId: targetID,
			Bidirectional:   bidirectional,
			NoCache:         req.Msg.GetNoCache(),
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

	materials := []attestation.Artifact{{URI: fmt.Sprintf("catalog://%s/%s", identity.TenantID, sourceID)}}
	if targetID != sourceID {
		materials = append(materials, attestation.Artifact{URI: fmt.Sprintf("catalog://%s/%s", identity.TenantID, targetID)})
	}
	s.emitAttestation(ctx, "gateway.StartAnalysis",
		materials,
		[]attestation.Artifact{{URI: fmt.Sprintf("job://%s/%s", identity.TenantID, jobID)}},
		nil)

	s.recordMetrics(ctx, "StartAnalysis", start, connect.Code(0))
	return connect.NewResponse(&pb.StartAnalysisResponse{
		JobId:  jobID,
		Status: jobResp.Msg.GetStatus(),
	}), nil
}
