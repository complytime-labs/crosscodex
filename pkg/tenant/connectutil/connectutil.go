package connectutil

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

const (
	// MetadataKeyTenantID is the Connect request header for tenant identity.
	// Same value as pkg/tenant/grpcutil.MetadataKeyTenantID.
	MetadataKeyTenantID = "x-tenant-id"

	// MetadataKeyUserID is the Connect request header for user identity.
	// Same value as pkg/tenant/grpcutil.MetadataKeyUserID.
	MetadataKeyUserID = "x-user-id"
)

// UnaryClientInterceptor propagates the ambient tenant (and, if present,
// user) from ctx onto outgoing Connect request headers. Fails closed with
// CodeFailedPrecondition if no tenant is present in ctx.
func UnaryClientInterceptor() connect.Interceptor {
	return &clientInterceptor{}
}

type clientInterceptor struct{}

func (clientInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		tenantID, err := tenant.FromContext(ctx)
		if err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("no tenant in context: %w", err))
		}
		req.Header().Set(MetadataKeyTenantID, tenantID)
		if userID := tenant.UserFromContext(ctx); userID != "" {
			req.Header().Set(MetadataKeyUserID, userID)
		}
		return next(ctx, req)
	}
}

func (clientInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (clientInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// UnaryServerInterceptor extracts the tenant (and optional user) header set
// by UnaryClientInterceptor and injects it into ctx via tenant.WithTenant.
//
// The headers are an assertion of identity, so they are honored only when the
// peer's TLS client certificate (read from authn.TLSStateFromContext, which
// the server must populate with authn.TLSStateMiddleware) matches one of
// identities. Fails closed: no TLS state or no peer certificate is
// CodeUnauthenticated; a certificate matching no identity (including an empty
// identities list) is CodePermissionDenied; a missing or invalid tenant
// header is CodeUnauthenticated. The handler is never reached on any failure.
func UnaryServerInterceptor(identities []authn.X509Match) connect.Interceptor {
	return &serverInterceptor{identities: identities}
}

type serverInterceptor struct {
	identities []authn.X509Match
}

// authorizePeer returns nil when ctx carries a client certificate matching a
// service identity.
func (i serverInterceptor) authorizePeer(ctx context.Context) error {
	state := authn.TLSStateFromContext(ctx)
	if state == nil || len(state.PeerCertificates) == 0 {
		return connect.NewError(connect.CodeUnauthenticated,
			errors.New("tenant headers require a TLS client certificate, but none was presented. Connect over mTLS with a certificate listed in tls.service_identities.")) //nolint:staticcheck // ST1005: the exact sentence is the specified, user-facing message
	}
	leaf := state.PeerCertificates[0]
	for _, id := range i.identities {
		if id.Matches(leaf) {
			return nil
		}
	}
	return connect.NewError(connect.CodePermissionDenied,
		fmt.Errorf("tenant headers are accepted only from a service identity: the client certificate CN=%s is not listed in tls.service_identities. Add the gateway's client certificate subject there.", leaf.Subject.CommonName)) //nolint:staticcheck // ST1005: the exact sentence is the specified, user-facing message
}

func (i serverInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := i.authorizePeer(ctx); err != nil {
			return nil, err
		}
		tenantID := req.Header().Get(MetadataKeyTenantID)
		if tenantID == "" {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing tenant header"))
		}
		ctx, err := tenant.WithTenant(ctx, tenantID)
		if err != nil {
			return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("invalid tenant: %w", err))
		}
		if userID := req.Header().Get(MetadataKeyUserID); userID != "" {
			ctx = tenant.WithUser(ctx, userID)
		}
		return next(ctx, req)
	}
}

func (serverInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler rejects every streaming RPC: no service behind this
// interceptor defines one, and a pass-through would skip the identity and
// tenant checks. Fails closed until streaming gets the same checks.
func (serverInterceptor) WrapStreamingHandler(connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(context.Context, connect.StreamingHandlerConn) error {
		return connect.NewError(connect.CodeUnimplemented,
			errors.New("streaming RPCs are not supported on tenant-scoped services"))
	}
}

var (
	_ connect.Interceptor = (*clientInterceptor)(nil)
	_ connect.Interceptor = (*serverInterceptor)(nil)
)
