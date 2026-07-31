package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"

	"connectrpc.com/connect"
	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type ctxKey int

const (
	ctxKeyTLSState ctxKey = iota
	ctxKeyClientIP
)

func tlsStateFromContext(ctx context.Context) *tls.ConnectionState {
	state, _ := ctx.Value(ctxKeyTLSState).(*tls.ConnectionState)
	return state
}

func clientIPFromContext(ctx context.Context) string {
	ip, _ := ctx.Value(ctxKeyClientIP).(string)
	return ip
}

func tlsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if r.TLS != nil {
			ctx = context.WithValue(ctx, ctxKeyTLSState, r.TLS)
		}
		ctx = context.WithValue(ctx, ctxKeyClientIP, r.RemoteAddr)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func isHealthProc(procedure string) bool {
	return procedure == crosscodexv1connect.GatewayServiceHealthProcedure
}

func identityFromContext(ctx context.Context) *authn.Identity {
	return authn.IdentityFromContext(ctx)
}

type connectAuthInterceptor struct {
	service *Service
}

func (s *Service) connectAuthInterceptor() connect.Interceptor {
	return &connectAuthInterceptor{service: s}
}

func (i *connectAuthInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if !req.Spec().IsClient && isHealthProc(req.Spec().Procedure) {
			return next(ctx, req)
		}

		tlsState := tlsStateFromContext(ctx)
		if tlsState == nil {
			i.service.recordAuthFailure(ctx, "no_tls")
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("TLS required"))
		}

		authReq := &authn.Request{
			Method:   authn.AuthMethodMTLS,
			TLSState: tlsState,
			ClientIP: clientIPFromContext(ctx),
		}

		identity, err := i.service.authn.Authenticate(ctx, authReq)
		if err != nil {
			i.service.recordAuthFailure(ctx, "auth_failed")
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("authentication failed"))
		}

		ctx = authn.WithIdentity(ctx, identity)

		ctx, err = tenant.WithTenant(ctx, identity.TenantID)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("invalid tenant: %w", err))
		}
		ctx = tenant.WithUser(ctx, identity.Subject)

		return next(ctx, req)
	}
}

func (i *connectAuthInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *connectAuthInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if !conn.Spec().IsClient && isHealthProc(conn.Spec().Procedure) {
			return next(ctx, conn)
		}

		tlsState := tlsStateFromContext(ctx)
		if tlsState == nil {
			i.service.recordAuthFailure(ctx, "no_tls")
			return connect.NewError(connect.CodeUnauthenticated, errors.New("TLS required"))
		}

		authReq := &authn.Request{
			Method:   authn.AuthMethodMTLS,
			TLSState: tlsState,
			ClientIP: clientIPFromContext(ctx),
		}

		identity, err := i.service.authn.Authenticate(ctx, authReq)
		if err != nil {
			i.service.recordAuthFailure(ctx, "auth_failed")
			return connect.NewError(connect.CodeUnauthenticated, errors.New("authentication failed"))
		}

		ctx = authn.WithIdentity(ctx, identity)

		ctx, err = tenant.WithTenant(ctx, identity.TenantID)
		if err != nil {
			return connect.NewError(connect.CodeInternal, fmt.Errorf("invalid tenant: %w", err))
		}
		ctx = tenant.WithUser(ctx, identity.Subject)

		return next(ctx, conn)
	}
}

func (s *Service) recordAuthFailure(ctx context.Context, reason string) {
	if s.authFailures != nil {
		s.authFailures.Add(ctx, 1,
			metric.WithAttributes(attribute.String("reason", reason)))
	}
}
