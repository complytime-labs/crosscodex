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
	service      *Service
	tenantStatus TenantStatusChecker
}

func (s *Service) connectAuthInterceptor(tenantStatus TenantStatusChecker) connect.Interceptor {
	return &connectAuthInterceptor{service: s, tenantStatus: tenantStatus}
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
		if err := i.checkTenantActive(ctx, identity.TenantID); err != nil {
			return nil, err
		}

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
		if err := i.checkTenantActive(ctx, identity.TenantID); err != nil {
			return err
		}

		return next(ctx, conn)
	}
}

// checkTenantActive refuses a tenant that is suspended or has no tenants
// row. A failed lookup also refuses (fail closed); its detail is logged here
// and withheld from the caller (CWE-209).
func (i *connectAuthInterceptor) checkTenantActive(ctx context.Context, tenantID string) error {
	active, err := i.tenantStatus.TenantActive(ctx, tenantID)
	if err != nil {
		i.service.logger.ErrorContext(ctx, "tenant status check failed", "tenant", tenantID, "error", err)
		i.service.recordAuthFailure(ctx, "tenant_status_error")
		return connect.NewError(connect.CodeUnavailable, errors.New("tenant status could not be checked; retry later"))
	}
	if !active {
		i.service.recordAuthFailure(ctx, "tenant_inactive")
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf(
			"tenant %q is suspended or not provisioned; requests are refused until an operator runs 'crosscodexd admin tenant resume --tenant %s' (or 'crosscodexd admin tenant create' if it does not exist)",
			tenantID, tenantID))
	}
	return nil
}

func (s *Service) recordAuthFailure(ctx context.Context, reason string) {
	if s.authFailures != nil {
		s.authFailures.Add(ctx, 1,
			metric.WithAttributes(attribute.String("reason", reason)))
	}
}
