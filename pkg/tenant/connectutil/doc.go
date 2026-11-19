// Package connectutil provides Connect-RPC interceptors for tenant context
// propagation, mirroring pkg/tenant/grpcutil's shape for the raw-gRPC
// transport. The client interceptor extracts tenant identity from context
// and sets it as an outgoing request header; the server interceptor does
// the reverse, injecting it into the incoming request's context via
// tenant.WithTenant. The server interceptor honours x-tenant-id, x-user-id
// and x-user-roles only from a peer whose client certificate matches one of
// its service identities (tls.service_identities), and needs the server
// wrapped in authn.TLSStateMiddleware to see that certificate; without it
// every request is refused.
//
// The end user is asserted atomically: the client sends x-user-id and
// x-user-roles together from the authn.Identity in ctx, and the server turns
// them into an authn.Identity with Method authn.AuthMethodServiceAsserted (and
// tenant.WithUser). Either header alone, an empty one or an unknown role is
// refused, so a far-side role check never sees a user without its roles.
//
// This exists because a real network hop between two Connect-RPC services
// (e.g. internal/gateway calling a standalone internal/pipeline.Server)
// does not share an in-process context.Context -- the identity that
// internal/gateway's own mTLS auth interceptor put in ctx must be
// re-asserted explicitly on the wire for the far side to see it.
package connectutil
