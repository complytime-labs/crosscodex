package connectutil

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

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
// Fails closed with CodeUnauthenticated if the tenant header is missing or
// invalid.
func UnaryServerInterceptor() connect.Interceptor {
	return &serverInterceptor{}
}

type serverInterceptor struct{}

func (serverInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
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

func (serverInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

var (
	_ connect.Interceptor = (*clientInterceptor)(nil)
	_ connect.Interceptor = (*serverInterceptor)(nil)
)
