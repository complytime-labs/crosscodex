package gateway

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/rpcserver"
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

	convertReq := &pb.ConvertDocumentRequest{
		Metadata: req.Msg.GetMetadata(),
		Mapping:  req.Msg.GetIngestionMapping(),
	}
	switch src := req.Msg.Source.(type) {
	case *pb.SubmitDocumentRequest_Content:
		convertReq.Source = &pb.ConvertDocumentRequest_Content{Content: src.Content}
	case *pb.SubmitDocumentRequest_SourceUri:
		convertReq.Source = &pb.ConvertDocumentRequest_SourceUri{SourceUri: src.SourceUri}
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("unknown source type"))
	}

	return s.processDocument(ctx, start, processDocumentInput{
		convertReq:    convertReq,
		catalogFormat: req.Msg.GetCatalogFormat(),
		catalogName:   req.Msg.GetCatalogName(),
		documentSlug:  req.Msg.GetDocumentSlug(),
		versionLabel:  req.Msg.GetVersionLabel(),
		rpcName:       "SubmitDocument",
	})
}

// maxMappingIDLen and mappingIDPattern are crosscodex-ingestion's rule for a
// mapping ID (its mapping-v1 schema), the ID of a mapping registered on the
// ingestion server.
const maxMappingIDLen = 128

var mappingIDPattern = regexp.MustCompile(`^[a-z0-9]+([.-][a-z0-9]+)*$`)

// maxMappingEcho bounds how much of a refused mapping ID an error quotes.
const maxMappingEcho = 64

// ValidateIngestionMapping checks that a non-empty mapping is well-formed
// under crosscodex-ingestion's mapping-ID rule; the empty string, no mapping,
// is valid. Whether the ID is registered is for the ingestion server to say.
// The error quotes at most maxMappingEcho bytes of mapping, escaped.
func ValidateIngestionMapping(mapping string) error {
	if mapping == "" || (len(mapping) <= maxMappingIDLen && mappingIDPattern.MatchString(mapping)) {
		return nil
	}
	quoted := fmt.Sprintf("%q", mapping)
	if len(mapping) > maxMappingEcho {
		quoted = fmt.Sprintf("%q (%d bytes)", mapping[:maxMappingEcho]+"...", len(mapping))
	}
	return fmt.Errorf("invalid mapping %s: a mapping ID is 1-128 lowercase letters and digits, separated by single '.' or '-'. "+
		"Give the ID of a mapping registered on the ingestion server (for example owasp-asvs-5.0.0-json), "+
		"or leave the mapping out to import OSCAL JSON or a document", quoted)
}

type processDocumentInput struct {
	convertReq    *pb.ConvertDocumentRequest
	catalogFormat pb.CatalogFormat
	catalogName   string
	documentSlug  string
	versionLabel  string
	rpcName       string
}

func (s *Service) processDocument(ctx context.Context, start time.Time, input processDocumentInput) (*connect.Response[pb.SubmitDocumentResponse], error) {
	if s.ingestion == nil || s.catalog == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("required backends not configured"))
	}

	// Ingestion stores what it receives, so an empty upload must be refused
	// here or it leaves an empty blob behind and fails later with a misleading
	// format error.
	if src, ok := input.convertReq.Source.(*pb.ConvertDocumentRequest_Content); ok && len(src.Content) == 0 {
		s.recordMetrics(ctx, input.rpcName, start, connect.CodeInvalidArgument)
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(
			"document is empty: the upload contained no content bytes, so there is nothing to import. "+
				"Send the catalog file's contents (for example, an OSCAL JSON catalog) with the request"))
	}
	if err := ValidateIngestionMapping(input.convertReq.GetMapping()); err != nil {
		s.recordMetrics(ctx, input.rpcName, start, connect.CodeInvalidArgument)
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	// ParseCatalog would reject the converted structured source as OSCAL or
	// Gemara, after the conversion had stored a blob.
	if mapping := input.convertReq.GetMapping(); mapping != "" && input.catalogFormat != pb.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED {
		s.recordMetrics(ctx, input.rpcName, start, connect.CodeInvalidArgument)
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
			"mapping %q cannot be combined with catalog format %s: a mapping converts a structured JSON, CSV or XLSX source, "+
				"which is not an OSCAL or Gemara catalog. Leave the catalog format unspecified to import through the mapping, "+
				"or leave the mapping out to import the file as %s",
			mapping, input.catalogFormat, input.catalogFormat))
	}

	identity := identityFromContext(ctx)
	tc := buildTenantContext(identity)
	input.convertReq.TenantContext = tc

	// Step 1: Convert document to markdown
	convertResp, err := s.ingestion.ConvertDocument(ctx, connect.NewRequest(input.convertReq))
	if err != nil {
		err = s.publicError(ctx, input.rpcName, err)
		s.recordMetrics(ctx, input.rpcName, start, connect.CodeOf(err))
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
		Format:        input.catalogFormat,
		CatalogName:   input.catalogName,
		DocumentSlug:  input.documentSlug,
		VersionLabel:  input.versionLabel,
		// The catalog service refuses converted output that names another mapping.
		IngestionMapping: input.convertReq.GetMapping(),
	}

	parseResp, err := s.catalog.ParseCatalog(ctx, connect.NewRequest(parseReq))
	if err != nil {
		if catalogRejected(err) {
			// WithoutCancel: a client that disconnected must not strand the blob.
			if discardErr := s.ingestion.DiscardDocument(context.WithoutCancel(ctx), docID); discardErr != nil {
				s.logger.Warn("discard rejected document failed; blob left in storage",
					"document_id", docID, "error", discardErr)
			}
		}
		err = s.publicError(ctx, input.rpcName, err)
		s.recordMetrics(ctx, input.rpcName, start, connect.CodeOf(err))
		return nil, err
	}

	catalogID := parseResp.Msg.GetCatalogId()
	if catalogID == "" {
		return nil, connect.NewError(connect.CodeInternal, errors.New("catalog backend returned empty catalog_id"))
	}

	byProducts := map[string]any{
		"catalog_format": input.catalogFormat.String(),
		"catalog_name":   input.catalogName,
	}
	if input.documentSlug != "" {
		byProducts["document_slug"] = input.documentSlug
		byProducts["version_label"] = parseResp.Msg.GetVersionLabel()
		byProducts["version"] = parseResp.Msg.GetVersion()
	}
	// The mapping decides what a structured source's controls are.
	if mapping := input.convertReq.GetMapping(); mapping != "" {
		byProducts["ingestion_mapping"] = mapping
		byProducts["ingestion_mapping_sha256"] = parseResp.Msg.GetIngestionMappingSha256()
	}
	// Empty for OSCAL and Gemara, which have no structure to attest.
	structureMethod := parseResp.Msg.GetIngestionStructureMethod()
	if structureMethod != "" {
		byProducts["ingestion_structure_method"] = structureMethod
	}
	if err := s.attestIngest(ctx, "gateway."+input.rpcName, docID, catalogID, input.convertReq.GetContent(), byProducts); err != nil {
		// The cause can name storage buckets, hosts or key files (CWE-209):
		// it is logged here, once, and the caller gets a reference to it.
		ref := rpcserver.NewErrorReference()
		s.logger.ErrorContext(ctx, "catalog import attestation failed", "method", input.rpcName, "error_ref", ref,
			"trace_id", rpcserver.LocalTraceID(ctx), "tenant", identity.TenantID, "subject", identity.Subject,
			"document_id", docID, "catalog_id", catalogID, "error", err)
		span := trace.SpanFromContext(ctx)
		span.RecordError(err)
		span.SetStatus(codes.Error, "catalog import attestation failed, error_ref "+ref)
		s.recordMetrics(ctx, input.rpcName, start, connect.CodeUnavailable)
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf(
			"catalog %q was imported but not attested: its in-toto ingest attestation could not be signed or stored, "+
				"so nothing yet proves which upload produced it. To attest it, re-run the same import; "+
				"if that keeps failing, give error reference %s to an operator", catalogID, ref))
	}

	s.recordMetrics(ctx, input.rpcName, start, connect.Code(0))

	return connect.NewResponse(&pb.SubmitDocumentResponse{
		DocumentId:       docID,
		CatalogId:        catalogID,
		TextlessControls: parseResp.Msg.GetTextlessControls(),

		Version:                  parseResp.Msg.GetVersion(),
		SupersededCatalogId:      parseResp.Msg.GetSupersededCatalogId(),
		ChangeReport:             parseResp.Msg.GetChangeReport(),
		IngestionStructureMethod: structureMethod,
	}), nil
}

// catalogRejected reports whether ParseCatalog refused the document itself,
// which it does before persisting anything, so no catalog row can reference
// the stored blob. Any other failure (Internal, Unavailable, ...) may follow a
// persisted catalog row that points at the blob, so the blob is kept.
func catalogRejected(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodeInvalidArgument, connect.CodeFailedPrecondition, connect.CodeUnimplemented:
		return true
	default:
		return false
	}
}
