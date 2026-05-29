package authn

import "context"

// Authenticator handles authentication requests.
//
// Implementations must validate credentials and extract identity.
// Return ErrUnsupportedMethod if the request type does not match
// (e.g., no TLSState for an X.509 authenticator). The Registry
// uses this sentinel to try the next authenticator.
type Authenticator interface {
	// Authenticate validates credentials and returns the authenticated identity.
	Authenticate(ctx context.Context, req *Request) (*Identity, error)

	// SupportedMethods returns the authentication methods this authenticator handles.
	SupportedMethods() []AuthMethod
}

// AuditEmitter emits authentication events for audit logging.
// The Gateway provides a natsbus-backed implementation; pkg/authn
// never imports pkg/natsbus directly.
type AuditEmitter interface {
	EmitAuthEvent(ctx context.Context, event *AuthEvent) error
}
