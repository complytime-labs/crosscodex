package gateway

import (
	"context"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func identityFromContext(ctx context.Context) *authn.Identity {
	return authn.IdentityFromContext(ctx)
}

func (s *Service) authInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		if info.FullMethod == pb.GatewayService_Health_FullMethodName {
			return handler(ctx, req)
		}

		p, ok := peer.FromContext(ctx)
		if !ok {
			s.recordAuthFailure(ctx, "no_peer")
			return nil, status.Error(codes.Unauthenticated, "TLS required")
		}

		tlsAuth, ok := p.AuthInfo.(credentials.TLSInfo)
		if !ok {
			s.recordAuthFailure(ctx, "no_tls")
			return nil, status.Error(codes.Unauthenticated, "TLS required")
		}

		peerAddr := ""
		if p.Addr != nil {
			peerAddr = p.Addr.String()
		}

		authReq := &authn.Request{
			Method:   authn.AuthMethodMTLS,
			TLSState: &tlsAuth.State,
			ClientIP: peerAddr,
		}

		identity, err := s.authn.Authenticate(ctx, authReq)
		if err != nil {
			s.recordAuthFailure(ctx, "auth_failed")
			return nil, status.Error(codes.Unauthenticated, "authentication failed")
		}

		ctx = authn.WithIdentity(ctx, identity)

		ctx, err = tenant.WithTenant(ctx, identity.TenantID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "invalid tenant: %v", err)
		}
		ctx = tenant.WithUser(ctx, identity.Subject)

		return handler(ctx, req)
	}
}

func (s *Service) recordAuthFailure(ctx context.Context, reason string) {
	if s.authFailures != nil {
		s.authFailures.Add(ctx, 1,
			metric.WithAttributes(attribute.String("reason", reason)))
	}
}
