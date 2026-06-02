// Package grpcutil provides gRPC interceptors for tenant context propagation.
//
// The server interceptor extracts tenant identity from incoming gRPC metadata
// and injects it into the request context via [tenant.WithTenant]. The client
// interceptor does the reverse — extracting from context and injecting into
// outgoing metadata.
//
// This package exists as a sub-package so that the core [tenant] package
// remains zero-dependency (no gRPC import).
package grpcutil
