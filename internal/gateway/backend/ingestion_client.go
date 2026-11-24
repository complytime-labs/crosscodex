package backend

import (
	"context"
	"errors"
	"fmt"
	"time"

	connect "connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// Conversion outcomes, the "outcome" attribute of the conversions counter.
const (
	outcomeCompleted = "completed" // output stored and verified
	outcomeRejected  = "rejected"  // refused before any RPC (size, source kind, tenant)
	outcomeFailed    = "failed"    // ingestion reported a terminal non-success status
	outcomeTimeout   = "timeout"   // ingestion.convert_timeout elapsed
	outcomeCanceled  = "canceled"  // the caller's context ended first
	outcomeError     = "error"     // RPC, URI resolution or storage failure
)

// IngestionClient converts documents through the crosscodex-ingestion
// (Docling) service. It implements gateway.IngestionBackend.
//
// Errors it returns may carry ingestion's own error text so the gateway's
// publicError can log it; every such error has a code publicError withholds
// (Internal, Unavailable or DeadlineExceeded), so that text never reaches the
// caller (CWE-209). Only refusals this client makes itself, with its own
// message, use caller-actionable codes.
type IngestionClient struct {
	rpc   crosscodexv1connect.IngestionServiceClient
	store storage.Provider
	sb    StorageBackend
	cfg   config.IngestionConfig

	tracer      trace.Tracer
	meter       metric.Meter
	conversions metric.Int64Counter
	duration    metric.Int64Histogram
}

// IngestionClientOption configures an IngestionClient.
type IngestionClientOption func(*IngestionClient)

// WithIngestionClientTelemetry sets the tracer and meter. Wiring contract: the
// composition root passes telemetry.Instrumentation("internal/gateway/backend").
// Without it the client records no spans or metrics.
func WithIngestionClientTelemetry(t trace.Tracer, m metric.Meter) IngestionClientOption {
	return func(c *IngestionClient) {
		c.tracer = t
		c.meter = m
	}
}

// NewIngestionClient returns an ingestion backend that sends inline content
// to the ingestion service and returns the converted output's storage key as
// the document ID.
//
// Wiring contract:
//   - rpc speaks gRPC (connect.WithGRPC) to config ingestion.address, over the
//     TLS client config pkg/tlsconfig builds for tls.targets.ingestion.
//   - store is the same tenant-routing storage.Provider the catalog backend
//     reads documents from; keys are tenant-relative.
//   - sb describes the storage backend ingestion writes to, as ingestion names
//     it in its URIs. For Kind "local", Root must be absolute and be the root
//     path inside the ingestion service's file:// URIs (deployments mount the
//     shared volume at the same path in both services). A relative Root fails
//     every conversion with an internal error.
//   - cfg is the ingestion config block; it is validated here.
func NewIngestionClient(rpc crosscodexv1connect.IngestionServiceClient, store storage.Provider, sb StorageBackend, cfg config.IngestionConfig, opts ...IngestionClientOption) (*IngestionClient, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("ingestion client: %w", err)
	}
	c := &IngestionClient{rpc: rpc, store: store, sb: sb, cfg: cfg}
	for _, opt := range opts {
		opt(c)
	}
	if c.meter == nil {
		c.meter = metricnoop.NewMeterProvider().Meter("")
	}
	var err error
	if c.conversions, err = c.meter.Int64Counter("crosscodex.ingestion.conversions.total",
		metric.WithDescription("Document conversions through the ingestion service, by outcome")); err != nil {
		return nil, fmt.Errorf("ingestion client: create conversions counter: %w", err)
	}
	if c.duration, err = c.meter.Int64Histogram("crosscodex.ingestion.conversion.duration_ms",
		metric.WithDescription("Time from request to verified output, by outcome"), metric.WithUnit("ms")); err != nil {
		return nil, fmt.Errorf("ingestion client: create duration histogram: %w", err)
	}
	return c, nil
}

// ConvertDocument sends the request's inline content for conversion, polls
// until ingestion finishes, and returns the output's tenant-relative storage
// key as document_id with status COMPLETED. The whole call is bounded by
// ingestion.convert_timeout.
func (c *IngestionClient) ConvertDocument(ctx context.Context, req *connect.Request[pb.ConvertDocumentRequest]) (*connect.Response[pb.ConvertDocumentResponse], error) {
	start := time.Now()
	ctx, span := telemetry.StartSpan(c.tracer, ctx, "IngestionClient.ConvertDocument")
	defer span.End()
	span.SetAttributes(attribute.String("operation", "convert_document"))

	key, outcome, err := c.convert(ctx, span, req.Msg)
	attrs := metric.WithAttributes(attribute.String("outcome", outcome))
	c.conversions.Add(ctx, 1, attrs)
	c.duration.Record(ctx, time.Since(start).Milliseconds(), attrs)
	span.SetAttributes(attribute.String("outcome", outcome))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, outcome)
		return nil, err
	}
	return connect.NewResponse(&pb.ConvertDocumentResponse{
		DocumentId: key,
		Status:     pb.JobStatus_JOB_STATUS_COMPLETED,
	}), nil
}

func (c *IngestionClient) convert(ctx context.Context, span trace.Span, msg *pb.ConvertDocumentRequest) (string, string, error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return "", outcomeRejected, connect.NewError(connect.CodeUnauthenticated, errors.New("no tenant in context"))
	}
	span.SetAttributes(attribute.String("tenant.id", tenantID))

	src, ok := msg.GetSource().(*pb.ConvertDocumentRequest_Content)
	if !ok {
		return "", outcomeRejected, connect.NewError(connect.CodeUnimplemented, errors.New(
			"the ingestion service is given only uploaded content; source_uri is not implemented, so upload the document's content instead"))
	}
	if size := int64(len(src.Content)); size > c.cfg.MaxDocumentBytes {
		return "", outcomeRejected, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf(
			"document is %d bytes, more than ingestion.max_document_bytes (%d): split the document, or ask an operator to raise ingestion.max_document_bytes",
			size, c.cfg.MaxDocumentBytes))
	}

	// The tenant on ctx is authoritative; the caller's tenant context only
	// contributes its metadata.
	tc := &pb.TenantContext{TenantId: tenantID, Metadata: msg.GetTenantContext().GetMetadata()}

	convCtx, cancel := context.WithTimeout(ctx, c.cfg.ConvertTimeout)
	defer cancel()

	resp, err := c.rpc.ConvertDocument(convCtx, connect.NewRequest(&pb.ConvertDocumentRequest{
		TenantContext: tc,
		Source:        src,
		Metadata:      msg.GetMetadata(),
	}))
	if err != nil {
		outcome, err := c.rpcFailure(ctx, convCtx, "ConvertDocument", "", err)
		return "", outcome, err
	}
	docID := resp.Msg.GetDocumentId()
	if docID == "" {
		return "", outcomeError, connect.NewError(connect.CodeInternal, errors.New(
			"ingestion ConvertDocument returned an empty document_id; check the ingestion service version"))
	}
	span.SetAttributes(attribute.String("ingestion.document_id", docID))

	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()
	var doc *pb.Document
	for doc == nil {
		select {
		case <-convCtx.Done():
			outcome, err := c.rpcFailure(ctx, convCtx, "GetDocument", docID, convCtx.Err())
			return "", outcome, err
		case <-ticker.C:
		}
		got, err := c.rpc.GetDocument(convCtx, connect.NewRequest(&pb.GetDocumentRequest{TenantContext: tc, DocumentId: docID}))
		if err != nil {
			outcome, err := c.rpcFailure(ctx, convCtx, "GetDocument", docID, err)
			return "", outcome, err
		}
		switch status := got.Msg.GetDocument().GetConversionStatus(); status {
		case pb.JobStatus_JOB_STATUS_PENDING, pb.JobStatus_JOB_STATUS_RUNNING:
		case pb.JobStatus_JOB_STATUS_COMPLETED:
			doc = got.Msg.GetDocument()
		default:
			return "", outcomeFailed, connect.NewError(connect.CodeInternal, fmt.Errorf(
				"ingestion could not convert document %q (status %s): %s",
				docID, status, got.Msg.GetDocument().GetError().GetMessage()))
		}
	}

	// The URI is service-supplied and untrusted. Neither it nor the resolver's
	// input goes into the error: ResolveDocumentKey's message names no tenant
	// and no root.
	key, err := ResolveDocumentKey(ctx, doc.GetStructuredJsonUri(), c.sb)
	if err != nil {
		return "", outcomeError, connect.NewError(connect.CodeInternal, fmt.Errorf(
			"ingestion output for document %q is not usable: %w", docID, err))
	}
	exists, err := c.store.Exists(ctx, key)
	if err != nil {
		return "", outcomeError, connect.NewError(connect.CodeInternal, fmt.Errorf(
			"check ingestion output for document %q: %w", docID, err))
	}
	if !exists {
		return "", outcomeError, connect.NewError(connect.CodeInternal, fmt.Errorf(
			"ingestion reported document %q converted, but its output is not in this tenant's storage: "+
				"check that crosscodex and the ingestion service share the same storage backend (root or bucket)", docID))
	}
	return key, outcomeCompleted, nil
}

// rpcFailure classifies a failed call to ingestion, or the end of convCtx
// while polling, into an outcome and an error. ctx is the caller's context
// and convCtx the one bounded by ingestion.convert_timeout. An ingestion
// error never keeps a caller-actionable code: its text belongs in the log,
// and Internal or Unavailable makes publicError withhold it.
func (c *IngestionClient) rpcFailure(ctx, convCtx context.Context, rpc, docID string, err error) (string, error) {
	doc := "the document"
	if docID != "" {
		doc = fmt.Sprintf("document %q", docID)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		code := connect.CodeCanceled
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			code = connect.CodeDeadlineExceeded
		}
		return outcomeCanceled, connect.NewError(code, fmt.Errorf(
			"document conversion stopped because the request ended before ingestion finished: %w", ctxErr))
	}
	if errors.Is(convCtx.Err(), context.DeadlineExceeded) {
		return outcomeTimeout, connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf(
			"%s did not finish converting within ingestion.convert_timeout (%s): retry later, "+
				"or ask an operator to raise ingestion.convert_timeout or add ingestion workers", doc, c.cfg.ConvertTimeout))
	}
	code := connect.CodeInternal
	if connect.CodeOf(err) == connect.CodeUnavailable {
		code = connect.CodeUnavailable
	}
	return outcomeError, connect.NewError(code, fmt.Errorf("ingestion %s for %s failed: %w", rpc, doc, err))
}

// DiscardDocument deletes the ingestion output ConvertDocument returned as
// documentID. A document that is already gone is not an error.
func (c *IngestionClient) DiscardDocument(ctx context.Context, documentID string) error {
	ctx, span := telemetry.StartSpan(c.tracer, ctx, "IngestionClient.DiscardDocument")
	defer span.End()
	span.SetAttributes(attribute.String("operation", "discard_document"))
	if tenantID, err := tenant.FromContext(ctx); err == nil {
		span.SetAttributes(attribute.String("tenant.id", tenantID))
	}

	if err := c.store.Delete(ctx, documentID); err != nil && !errors.Is(err, storage.ErrNotFound) {
		span.RecordError(err)
		span.SetStatus(codes.Error, "delete failed")
		return fmt.Errorf("discard document %s: %w", documentID, err)
	}
	return nil
}
