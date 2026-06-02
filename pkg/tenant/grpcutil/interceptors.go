package grpcutil

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

const (
	// MetadataKeyTenantID is the gRPC metadata key for tenant identity.
	MetadataKeyTenantID = "x-tenant-id"

	// MetadataKeyUserID is the gRPC metadata key for user identity.
	MetadataKeyUserID = "x-user-id"
)

// UnaryServerInterceptor returns a gRPC server interceptor that extracts
// tenant identity from incoming metadata and injects it into the context.
//
// Missing tenant ID returns codes.Unauthenticated.
// Invalid tenant ID returns codes.InvalidArgument.
func UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "missing metadata")
		}

		tenantIDs := md.Get(MetadataKeyTenantID)
		if len(tenantIDs) == 0 || tenantIDs[0] == "" {
			return nil, status.Error(codes.Unauthenticated, "missing tenant ID in metadata")
		}

		ctx, err := tenant.WithTenant(ctx, tenantIDs[0])
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid tenant ID: %v", err)
		}

		userIDs := md.Get(MetadataKeyUserID)
		if len(userIDs) > 0 && userIDs[0] != "" {
			ctx = tenant.WithUser(ctx, userIDs[0])
		}

		return handler(ctx, req)
	}
}

// UnaryClientInterceptor returns a gRPC client interceptor that injects
// tenant identity from the context into outgoing metadata.
//
// Fails closed: returns an error if no tenant is present in the context.
func UnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req, reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		tenantID, err := tenant.FromContext(ctx)
		if err != nil {
			return status.Errorf(codes.FailedPrecondition, "no tenant in context: %v", err)
		}

		md, ok := metadata.FromOutgoingContext(ctx)
		if ok {
			md = md.Copy()
		} else {
			md = metadata.New(nil)
		}

		md.Set(MetadataKeyTenantID, tenantID)

		if userID := tenant.UserFromContext(ctx); userID != "" {
			md.Set(MetadataKeyUserID, userID)
		}

		ctx = metadata.NewOutgoingContext(ctx, md)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}
