package gateway

import (
	"context"
	"crypto/tls"

	"connectrpc.com/connect"
	"github.com/complytime-labs/crosscodex/pkg/authn"
)

// Export unexported functions for external test packages.

var ExportIdentityFromContext = identityFromContext
var ExportBuildTenantContext = buildTenantContext
var ExportHandleStreamedDocument = (*Service).handleStreamedDocument

// ExportContextWithIdentity injects an authn.Identity into a context
// using authn.WithIdentity, allowing external _test packages
// to set up authenticated contexts without the Connect auth interceptor.
func ExportContextWithIdentity(ctx context.Context, id *authn.Identity) context.Context {
	return authn.WithIdentity(ctx, id)
}

// ExportAuthInterceptor builds the Connect auth interceptor NewServer installs.
func ExportAuthInterceptor(s *Service, ts TenantStatusChecker) connect.Interceptor {
	return s.connectAuthInterceptor(ts)
}

// ExportContextWithTLSState stores a TLS state the way tlsMiddleware does.
func ExportContextWithTLSState(ctx context.Context, state *tls.ConnectionState) context.Context {
	return context.WithValue(ctx, ctxKeyTLSState, state)
}
