package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
)

func (s *Service) SubmitDocument(ctx context.Context, req *connect.Request[pb.SubmitDocumentRequest]) (*connect.Response[pb.SubmitDocumentResponse], error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}

	ctx, endSpan := s.startHandlerSpan(ctx, "SubmitDocument", identity)
	defer endSpan()

	if req.Msg.Source == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("source is required (content or source_uri)"))
	}

	if s.ingestion == nil || s.catalog == nil || s.pipeline == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("required backends not configured"))
	}

	tc := buildTenantContext(identity)

	// Step 1: Convert document to markdown
	convertReq := &pb.ConvertDocumentRequest{
		TenantContext: tc,
		Metadata:      req.Msg.GetMetadata(),
	}

	switch src := req.Msg.Source.(type) {
	case *pb.SubmitDocumentRequest_Content:
		convertReq.Source = &pb.ConvertDocumentRequest_Content{Content: src.Content}
	case *pb.SubmitDocumentRequest_SourceUri:
		convertReq.Source = &pb.ConvertDocumentRequest_SourceUri{SourceUri: src.SourceUri}
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("unknown source type"))
	}

	convertResp, err := s.ingestion.ConvertDocument(ctx, connect.NewRequest(convertReq))
	if err != nil {
		s.recordMetrics(ctx, "SubmitDocument", start, connect.CodeOf(err))
		return nil, err
	}

	docID := convertResp.Msg.GetDocumentId()
	if docID == "" {
		return nil, connect.NewError(connect.CodeInternal, errors.New("ingestion backend returned empty document_id"))
	}

	// Step 2: Parse catalog from converted markdown
	parseReq := &pb.ParseCatalogRequest{
		TenantContext: tc,
		DocumentId:    docID,
		Format:        req.Msg.GetCatalogFormat(),
		CatalogName:   req.Msg.GetCatalogName(),
	}

	parseResp, err := s.catalog.ParseCatalog(ctx, connect.NewRequest(parseReq))
	if err != nil {
		s.recordMetrics(ctx, "SubmitDocument", start, connect.CodeOf(err))
		return nil, err
	}

	catalogID := parseResp.Msg.GetCatalogId()
	if catalogID == "" {
		return nil, connect.NewError(connect.CodeInternal, errors.New("catalog backend returned empty catalog_id"))
	}

	// Step 3: Create full-analysis job
	jobReq := &pb.CreateJobRequest{
		TenantContext: tc,
		JobType:       pb.JobType_JOB_TYPE_FULL_ANALYSIS,
		Config: &pb.JobConfig{
			Source:          &pb.JobConfig_CatalogId{CatalogId: catalogID},
			CatalogFormat:   req.Msg.GetCatalogFormat(),
			CatalogName:     req.Msg.GetCatalogName(),
			TargetCatalogId: req.Msg.GetTargetCatalogId(),
			SynthesisConfig: req.Msg.GetSynthesisConfig(),
		},
	}

	jobResp, err := s.pipeline.CreateJob(ctx, connect.NewRequest(jobReq))
	if err != nil {
		s.recordMetrics(ctx, "SubmitDocument", start, connect.CodeOf(err))
		return nil, err
	}

	if jobResp.Msg.GetJobId() == "" {
		return nil, connect.NewError(connect.CodeInternal, errors.New("pipeline backend returned empty job_id"))
	}

	// Attestation: emit link for the 3-backend chain
	materials := []attestation.Artifact{
		{URI: fmt.Sprintf("document://%s/%s", identity.TenantID, docID), Digest: ""},
	}
	products := []attestation.Artifact{
		{URI: fmt.Sprintf("catalog://%s/%s", identity.TenantID, catalogID), Digest: ""},
		{URI: fmt.Sprintf("job://%s/%s", identity.TenantID, jobResp.Msg.GetJobId()), Digest: ""},
	}
	byProducts := map[string]any{
		"catalog_format": req.Msg.GetCatalogFormat().String(),
		"catalog_name":   req.Msg.GetCatalogName(),
	}
	if req.Msg.GetTargetCatalogId() != "" {
		byProducts["target_catalog_id"] = req.Msg.GetTargetCatalogId()
	}
	s.emitAttestation(ctx, "gateway.SubmitDocument", materials, products, byProducts)

	s.recordMetrics(ctx, "SubmitDocument", start, connect.Code(0))

	return connect.NewResponse(&pb.SubmitDocumentResponse{
		JobId:      jobResp.Msg.GetJobId(),
		DocumentId: docID,
		Status:     jobResp.Msg.GetStatus(),
	}), nil
}
