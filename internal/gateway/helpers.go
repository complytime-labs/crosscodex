package gateway

import (
	"bytes"
	"context"
	"fmt"

	"connectrpc.com/connect"

	"github.com/complytime-labs/crosscodex/internal/rpcserver"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// startHandlerSpan creates a tracing span with standard gateway handler
// attributes (rpc.method, tenant.id, user.id) plus any extra attributes.
// Returns the original context and a noop function if the tracer is nil.
// Callers must defer the returned function to end the span.
func (s *Service) startHandlerSpan(ctx context.Context, method string, identity *authn.Identity, extra ...attribute.KeyValue) (context.Context, func()) {
	if s.tracer == nil {
		return ctx, func() {}
	}
	attrs := make([]attribute.KeyValue, 0, 3+len(extra))
	attrs = append(attrs,
		attribute.String("rpc.method", method),
		attribute.String("tenant.id", identity.TenantID),
		attribute.String("user.id", identity.Subject),
	)
	attrs = append(attrs, extra...)
	ctx, span := s.tracer.Start(ctx, "gateway."+method, trace.WithAttributes(attrs...))
	return ctx, func() { span.End() }
}

// publicError turns an error from a backend, or from the request stream, into
// the error the gateway returns to its client. The gateway is the public
// boundary, so it does not trust backends to have sanitized their errors: a
// plain error (which Connect would send as CodeUnknown with its full text) or a
// Connect error of almost any code can carry SQL, driver messages, internal
// addresses, role names or file paths (CWE-209). Some backends wrap raw store
// errors in CodeInternal, and a dial failure arrives as CodeUnavailable.
//
// The policy is an allowlist, so an unclassified code fails closed. Only codes
// the caller can act on (invalid input, missing or conflicting resources,
// refused access, exhausted quota, its own cancellation, an unsupported
// feature) pass through unchanged. Every other error is logged here, once, and
// replaced by a fixed message naming a server-generated error reference. Transient codes
// (Unavailable, DeadlineExceeded, Aborted) keep their code so client retry
// logic still works; Unknown, Internal, DataLoss and plain errors become
// CodeInternal.
//
// The client message names a fresh server-generated error reference, which
// the caller cannot choose or omit, unlike a trace ID. The log record carries
// the reference, the trace ID when the span is this server's own (a remote
// one is caller-chosen), and the tenant and subject when known, so an operator
// can find the cause and who hit it.
func (s *Service) publicError(ctx context.Context, method string, err error) error {
	code := connect.CodeOf(err)
	switch code {
	case connect.CodeInvalidArgument, connect.CodeNotFound, connect.CodeAlreadyExists,
		connect.CodeFailedPrecondition, connect.CodeOutOfRange, connect.CodePermissionDenied,
		connect.CodeUnauthenticated, connect.CodeResourceExhausted, connect.CodeCanceled,
		connect.CodeUnimplemented:
		return err
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded, connect.CodeAborted:
		// Keep the code; only the message is withheld.
	default:
		code = connect.CodeInternal
	}
	ref := rpcserver.NewErrorReference()
	attrs := []any{"method", method, "error_ref", ref, "trace_id", rpcserver.LocalTraceID(ctx)}
	if tenantID, tenantErr := tenant.FromContext(ctx); tenantErr == nil {
		attrs = append(attrs, "tenant", tenantID)
	}
	if identity := identityFromContext(ctx); identity != nil {
		attrs = append(attrs, "subject", identity.Subject)
	}
	s.logger.ErrorContext(ctx, "gateway request failed", append(attrs, "error", err)...)
	return connect.NewError(code, fmt.Errorf(
		"internal gateway error; the server logged the cause under error reference %s. Retry the request, and if it keeps failing, give that reference to an operator", ref))
}

// publicErrorInterceptor applies publicError, with the procedure as the
// method, to every error a handler returns. It is for services mounted on the
// gateway server that do not apply the policy themselves (AdminService); the
// GatewayService handlers call publicError directly, so it is not installed on
// them and their errors are logged once. Install it innermost, after the auth
// interceptor, so it sees only handler errors and not the auth interceptor's
// own refusals.
type publicErrorInterceptor struct{ service *Service }

func (i publicErrorInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		resp, err := next(ctx, req)
		if err != nil {
			return nil, i.service.publicError(ctx, req.Spec().Procedure, err)
		}
		return resp, nil
	}
}

func (i publicErrorInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i publicErrorInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if err := next(ctx, conn); err != nil {
			return i.service.publicError(ctx, conn.Spec().Procedure, err)
		}
		return nil
	}
}

// attestIngest signs an in-toto link for one catalog import, step, with the
// generator WithAttestor resolves for the tenant on ctx, and stores it at
// attestation/ingest/<docID> (docID is the document's own storage key, such
// as documents/<uuid>.json) in that tenant's object store, under the
// attestation/ prefix retention collects. The link's material is the
// uploaded document, with the SHA-256 of content as its digest (also the
// content_hash byproduct); its product is the catalog. The generator adds
// the trace ID. It does nothing when no attestor is configured or the
// tenant's attestation is disabled, and otherwise fails closed: a tenant
// missing from ctx, an import with no uploaded bytes to digest, or a link
// that is not signed or stored is an error.
func (s *Service) attestIngest(ctx context.Context, step, docID, catalogID string, content []byte, byProducts map[string]any) error {
	if s.attestor == nil {
		return nil
	}
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return fmt.Errorf("resolve the tenant whose import to attest: %w", err)
	}
	gen := s.attestor(tenantID)
	if gen == nil {
		return nil
	}
	if len(content) == 0 {
		return fmt.Errorf("document %s has no uploaded bytes to digest", docID)
	}
	digest := storage.ContentHash(content)
	byProducts["content_hash"] = digest
	link, err := gen.CreateLink(ctx, step,
		[]attestation.Artifact{{URI: fmt.Sprintf("document://%s/%s", tenantID, docID), Digest: digest}},
		[]attestation.Artifact{{URI: fmt.Sprintf("catalog://%s/%s", tenantID, catalogID)}},
		attestation.WithByProducts(byProducts))
	if err != nil {
		return fmt.Errorf("sign the ingest link of document %s: %w", docID, err)
	}
	key := "attestation/ingest/" + docID
	if err := s.attestStore.Put(ctx, key, bytes.NewReader(link.Raw)); err != nil {
		return fmt.Errorf("store the ingest link at %s: %w", key, err)
	}
	return nil
}
