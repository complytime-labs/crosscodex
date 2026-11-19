package connectutil

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

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

	// MetadataKeyUserRoles is the Connect request header for the end user's
	// roles, comma-separated with no spaces (e.g. "admin,reviewer"). It always
	// travels with MetadataKeyUserID: the server refuses either one alone.
	MetadataKeyUserRoles = "x-user-roles"
)

// UnaryClientInterceptor propagates the ambient tenant, and the end user when
// ctx carries one, onto outgoing Connect request headers. The end user comes
// from the authn.Identity in ctx: its Subject becomes x-user-id and its Roles
// x-user-roles, always together.
//
// Fails closed with CodeFailedPrecondition, without sending the request, when
// ctx has no tenant, when ctx has a tenant.WithUser user but no Identity (its
// roles are unknown), when that user differs from Identity.Subject, or when
// the Identity's Subject is empty. Dropping the user instead would widen
// pkg/db's per-user row scope on the far side.
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
		user := tenant.UserFromContext(ctx)
		identity := authn.IdentityFromContext(ctx)
		switch {
		case identity == nil && user != "":
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(
				"the context carries a user but no authn.Identity, so the user's roles are unknown and cannot be asserted to the far side. "+
					"Put the caller's identity in the context with authn.WithIdentity, or call without a user"))
		case identity != nil && user != "" && user != identity.Subject:
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(
				"the context's user differs from its authn.Identity subject, so the end user to assert is ambiguous. "+
					"Set tenant.WithUser to the identity's Subject"))
		case identity != nil && identity.TenantID != tenantID:
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(
				"the authn.Identity in the context belongs to a different tenant than the request's context, so asserting it would grant the user another tenant's access. "+
					"Re-scope the context to the identity's tenant, or call without an identity"))
		case identity != nil && identity.Subject == "":
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(
				"the authn.Identity in the context has an empty subject, so the far side cannot scope the request to a user. "+
					"Authenticate the caller with a credential that names a subject (for mTLS, a certificate CN)"))
		}
		req.Header().Set(MetadataKeyTenantID, tenantID)
		if identity != nil {
			req.Header().Set(MetadataKeyUserID, identity.Subject)
			req.Header().Set(MetadataKeyUserRoles, strings.Join(identity.Roles, ","))
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

// UnaryServerInterceptor extracts the tenant, and the end user when asserted,
// from the headers set by UnaryClientInterceptor and injects them into ctx.
// The tenant goes in with tenant.WithTenant. An end user needs x-user-id and
// x-user-roles together; they become an authn.Identity (TenantID from
// x-tenant-id, Method authn.AuthMethodServiceAsserted) set with
// authn.WithIdentity, plus tenant.WithUser(subject). With neither header the
// handler sees the tenant only, no Identity and no user.
//
// The headers are an assertion of identity, so they are honored only when the
// peer's TLS client certificate (read from authn.TLSStateFromContext, which
// the server must populate with authn.TLSStateMiddleware) matches one of
// identities. Fails closed: no TLS state or no peer certificate is
// CodeUnauthenticated; a certificate matching no identity (including an empty
// identities list) is CodePermissionDenied; a missing or invalid tenant
// header is CodeUnauthenticated; exactly one of x-user-id and x-user-roles, a
// repeated or empty one, or a role list with an empty element or a role
// outside authn.KnownRoles is CodeInvalidArgument. Checks run in that order.
// The handler is never reached on any failure.
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
		fmt.Errorf("tenant headers are accepted only from a service identity: the client certificate CN=%s is not listed in tls.service_identities. If this certificate belongs to a role that calls this one (the gateway, or the graph role calling the pipeline role), add its subject there.", leaf.Subject.CommonName)) //nolint:staticcheck // ST1005: the exact sentence is the specified, user-facing message
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
		ctx, err = assertUser(ctx, req.Header(), tenantID)
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

// assertUser puts the end user asserted by x-user-id and x-user-roles into
// ctx, or returns ctx unchanged when neither header is present. Presence is
// tested on the header map, not on a non-empty value, so an empty header is
// refused rather than mistaken for an absent one.
func assertUser(ctx context.Context, header http.Header, tenantID string) (context.Context, error) {
	users := header.Values(MetadataKeyUserID)
	roleHeaders := header.Values(MetadataKeyUserRoles)
	switch {
	case len(users) == 0 && len(roleHeaders) == 0:
		return ctx, nil
	case len(roleHeaders) == 0:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(
			"x-user-id is present without x-user-roles: a service must assert the user and roles together. Send both headers, or neither"))
	case len(users) == 0:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(
			"x-user-roles is present without x-user-id: a service must assert the user and roles together. Send both headers, or neither"))
	case len(users) > 1:
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
			"x-user-id is sent %d times: a service asserts exactly one end user per request. Send the header once", len(users)))
	case len(roleHeaders) > 1:
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
			"x-user-roles is sent %d times: roles travel as one comma-separated header. Send the header once, such as \"admin,reviewer\"", len(roleHeaders)))
	case users[0] == "":
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(
			"x-user-id is empty: an asserted end user needs a subject. Send the user's subject, or neither x-user-id nor x-user-roles"))
	}
	roles, err := parseUserRoles(roleHeaders[0])
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	ctx = authn.WithIdentity(ctx, &authn.Identity{
		Subject:  users[0],
		TenantID: tenantID,
		Roles:    roles,
		Method:   authn.AuthMethodServiceAsserted,
	})
	return tenant.WithUser(ctx, users[0]), nil
}

// parseUserRoles parses an x-user-roles value: roles separated by "," with no
// trimming, each in authn.KnownRoles. It refuses an empty value, an empty
// element and an unknown role. Duplicates are allowed.
func parseUserRoles(header string) ([]string, error) {
	if header == "" {
		return nil, errors.New(
			"x-user-roles is empty: an asserted end user needs at least one role. Send the user's roles comma-separated, such as \"admin,reviewer\"")
	}
	roles := strings.Split(header, ",")
	for i, role := range roles {
		if role == "" {
			return nil, fmt.Errorf(
				"x-user-roles has an empty element at position %d: roles are separated by single commas with no empty entries. Send a list such as \"admin,reviewer\"", i+1)
		}
		if !slices.Contains(authn.KnownRoles, role) {
			return nil, fmt.Errorf("x-user-roles names unknown role %q: use one of %s", role, strings.Join(authn.KnownRoles, ", "))
		}
	}
	return roles, nil
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
