package gateway

import (
	"context"
	"fmt"
	"time"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Service) SubmitDocument(ctx context.Context, req *pb.SubmitDocumentRequest) (*pb.SubmitDocumentResponse, error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, status.Error(codes.Unauthenticated, "not authenticated")
	}

	if s.tracer != nil {
		var span trace.Span
		ctx, span = s.tracer.Start(ctx, "gateway.SubmitDocument",
			trace.WithAttributes(
				attribute.String("rpc.method", "SubmitDocument"),
				attribute.String("tenant.id", identity.TenantID),
				attribute.String("user.id", identity.Subject),
			))
		defer span.End()
	}

	if req.Source == nil {
		return nil, status.Error(codes.InvalidArgument, "source is required (content or source_uri)")
	}

	if s.ingestion == nil || s.catalog == nil || s.pipeline == nil {
		return nil, status.Error(codes.Unavailable, "required backends not configured")
	}

	tc := buildTenantContext(identity)

	// Step 1: Convert document to markdown
	convertReq := &pb.ConvertDocumentRequest{
		TenantContext: tc,
		Metadata:      req.GetMetadata(),
	}

	switch src := req.Source.(type) {
	case *pb.SubmitDocumentRequest_Content:
		convertReq.Source = &pb.ConvertDocumentRequest_Content{Content: src.Content}
	case *pb.SubmitDocumentRequest_SourceUri:
		convertReq.Source = &pb.ConvertDocumentRequest_SourceUri{SourceUri: src.SourceUri}
	default:
		return nil, status.Error(codes.InvalidArgument, "unknown source type")
	}

	convertResp, err := s.ingestion.ConvertDocument(ctx, convertReq)
	if err != nil {
		s.recordMetrics(ctx, "SubmitDocument", start, status.Code(err))
		return nil, err
	}

	docID := convertResp.GetDocumentId()
	if docID == "" {
		return nil, status.Error(codes.Internal, "ingestion backend returned empty document_id")
	}

	// Step 2: Parse catalog from converted markdown
	parseReq := &pb.ParseCatalogRequest{
		TenantContext: tc,
		DocumentId:    docID,
		Format:        req.GetCatalogFormat(),
		CatalogName:   req.GetCatalogName(),
	}

	parseResp, err := s.catalog.ParseCatalog(ctx, parseReq)
	if err != nil {
		s.recordMetrics(ctx, "SubmitDocument", start, status.Code(err))
		return nil, err
	}

	catalogID := parseResp.GetCatalogId()
	if catalogID == "" {
		return nil, status.Error(codes.Internal, "catalog backend returned empty catalog_id")
	}

	// Step 3: Create full-analysis job
	jobReq := &pb.CreateJobRequest{
		TenantContext: tc,
		JobType:       pb.JobType_JOB_TYPE_FULL_ANALYSIS,
		Config: &pb.JobConfig{
			Source:          &pb.JobConfig_CatalogId{CatalogId: catalogID},
			CatalogFormat:   req.GetCatalogFormat(),
			CatalogName:     req.GetCatalogName(),
			TargetCatalogId: req.GetTargetCatalogId(),
			SynthesisConfig: req.GetSynthesisConfig(),
		},
	}

	jobResp, err := s.pipeline.CreateJob(ctx, jobReq)
	if err != nil {
		s.recordMetrics(ctx, "SubmitDocument", start, status.Code(err))
		return nil, err
	}

	if jobResp.GetJobId() == "" {
		return nil, status.Error(codes.Internal, "pipeline backend returned empty job_id")
	}

	// Attestation: emit link for the 3-backend chain
	if s.attestor != nil {
		materials := []attestation.Artifact{
			{URI: fmt.Sprintf("document://%s/%s", identity.TenantID, docID), Digest: ""},
		}
		products := []attestation.Artifact{
			{URI: fmt.Sprintf("catalog://%s/%s", identity.TenantID, catalogID), Digest: ""},
			{URI: fmt.Sprintf("job://%s/%s", identity.TenantID, jobResp.GetJobId()), Digest: ""},
		}

		traceID := telemetry.TraceIDFromContext(ctx)
		byProducts := map[string]any{
			"catalog_format": req.GetCatalogFormat().String(),
			"catalog_name":   req.GetCatalogName(),
		}
		if req.GetTargetCatalogId() != "" {
			byProducts["target_catalog_id"] = req.GetTargetCatalogId()
		}

		link, err := s.attestor.CreateLink(ctx, "gateway.SubmitDocument", materials, products, attestation.WithByProducts(byProducts))
		if err != nil {
			// Log but don't fail the request
			if s.tracer != nil {
				trace.SpanFromContext(ctx).RecordError(err, trace.WithAttributes(
					attribute.String("error.type", "attestation_failed"),
					attribute.String("trace_id", traceID),
				))
			}
		} else {
			_ = link // Successfully created, stored by attestor
		}
	}

	s.recordMetrics(ctx, "SubmitDocument", start, codes.OK)

	return &pb.SubmitDocumentResponse{
		JobId:      jobResp.GetJobId(),
		DocumentId: docID,
		Status:     jobResp.GetStatus(),
	}, nil
}
