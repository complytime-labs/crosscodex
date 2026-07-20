package gateway

import (
	"context"

	"github.com/complytime-labs/crosscodex/pkg/authn"
)

// Export unexported functions for external test packages.

var ExportIdentityFromContext = identityFromContext
var ExportBuildTenantContext = buildTenantContext

// ExportContextWithIdentity injects an authn.Identity into a context
// using authn.WithIdentity, allowing external _test packages
// to set up authenticated contexts without the gRPC interceptor.
func ExportContextWithIdentity(ctx context.Context, id *authn.Identity) context.Context {
	return authn.WithIdentity(ctx, id)
}
