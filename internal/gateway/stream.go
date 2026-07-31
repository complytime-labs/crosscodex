package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
)

func (s *Service) StreamDocument(ctx context.Context, stream *connect.ClientStream[pb.StreamDocumentChunk]) (*connect.Response[pb.SubmitDocumentResponse], error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}

	ctx, endSpan := s.startHandlerSpan(ctx, "StreamDocument", identity)
	defer endSpan()

	var meta *pb.StreamDocumentMetadata
	var buf bytes.Buffer

	for stream.Receive() {
		chunk := stream.Msg()
		switch p := chunk.Payload.(type) {
		case *pb.StreamDocumentChunk_Metadata:
			if meta != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("metadata must appear exactly once as the first message"))
			}
			meta = p.Metadata
		case *pb.StreamDocumentChunk_Chunk:
			if meta == nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("first message must contain metadata"))
			}
			buf.Write(p.Chunk)
		}
	}
	if err := stream.Err(); err != nil {
		s.recordMetrics(ctx, "StreamDocument", start, connect.CodeOf(err))
		return nil, connect.NewError(connect.CodeUnknown, fmt.Errorf("stream error: %w", err))
	}

	if meta == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("first message must contain metadata"))
	}

	return s.handleStreamedDocument(ctx, start, meta, buf.Bytes())
}

func (s *Service) handleStreamedDocument(ctx context.Context, start time.Time, meta *pb.StreamDocumentMetadata, content []byte) (*connect.Response[pb.SubmitDocumentResponse], error) {
	// Identity already validated by StreamDocument caller; re-check here for defensive isolation (tests call directly)
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}

	if meta == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("metadata is required"))
	}

	if s.ingestion == nil || s.catalog == nil || s.pipeline == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("required backends not configured"))
	}

	tc := buildTenantContext(identity)

	convertReq := &pb.ConvertDocumentRequest{
		TenantContext: tc,
		Source:        &pb.ConvertDocumentRequest_Content{Content: content},
		Metadata:      meta.GetContentMetadata(),
	}

	convertResp, err := s.ingestion.ConvertDocument(ctx, convertReq)
	if err != nil {
		s.recordMetrics(ctx, "StreamDocument", start, connect.CodeOf(err))
		return nil, err
	}

	docID := convertResp.GetDocumentId()
	if docID == "" {
		return nil, connect.NewError(connect.CodeInternal, errors.New("ingestion backend returned empty document_id"))
	}

	parseReq := &pb.ParseCatalogRequest{
		TenantContext: tc,
		DocumentId:    docID,
		Format:        meta.GetCatalogFormat(),
		CatalogName:   meta.GetCatalogName(),
	}

	parseResp, err := s.catalog.ParseCatalog(ctx, parseReq)
	if err != nil {
		s.recordMetrics(ctx, "StreamDocument", start, connect.CodeOf(err))
		return nil, err
	}

	catalogID := parseResp.GetCatalogId()
	if catalogID == "" {
		return nil, connect.NewError(connect.CodeInternal, errors.New("catalog backend returned empty catalog_id"))
	}

	jobReq := &pb.CreateJobRequest{
		TenantContext: tc,
		JobType:       pb.JobType_JOB_TYPE_FULL_ANALYSIS,
		Config: &pb.JobConfig{
			Source:          &pb.JobConfig_CatalogId{CatalogId: catalogID},
			CatalogFormat:   meta.GetCatalogFormat(),
			CatalogName:     meta.GetCatalogName(),
			TargetCatalogId: meta.GetTargetCatalogId(),
			SynthesisConfig: meta.GetSynthesisConfig(),
		},
	}

	jobResp, err := s.pipeline.CreateJob(ctx, jobReq)
	if err != nil {
		s.recordMetrics(ctx, "StreamDocument", start, connect.CodeOf(err))
		return nil, err
	}

	if jobResp.GetJobId() == "" {
		return nil, connect.NewError(connect.CodeInternal, errors.New("pipeline backend returned empty job_id"))
	}

	materials := []attestation.Artifact{
		{URI: fmt.Sprintf("document://%s/%s", identity.TenantID, docID), Digest: ""},
	}
	products := []attestation.Artifact{
		{URI: fmt.Sprintf("catalog://%s/%s", identity.TenantID, catalogID), Digest: ""},
		{URI: fmt.Sprintf("job://%s/%s", identity.TenantID, jobResp.GetJobId()), Digest: ""},
	}
	byProducts := map[string]any{
		"catalog_format": meta.GetCatalogFormat().String(),
		"catalog_name":   meta.GetCatalogName(),
	}
	if meta.GetTargetCatalogId() != "" {
		byProducts["target_catalog_id"] = meta.GetTargetCatalogId()
	}
	s.emitAttestation(ctx, "gateway.StreamDocument", materials, products, byProducts)

	s.recordMetrics(ctx, "StreamDocument", start, connect.Code(0))

	return connect.NewResponse(&pb.SubmitDocumentResponse{
		JobId:      jobResp.GetJobId(),
		DocumentId: docID,
		Status:     jobResp.GetStatus(),
	}), nil
}
