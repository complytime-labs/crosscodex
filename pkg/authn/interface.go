package authn

import "context"

// Authenticator handles authentication requests.
//
// Implementations must validate credentials, extract identity,
// and map to tenant IDs.
type Authenticator interface {
	// Authenticate validates credentials and returns the authenticated identity.
	// Returns ErrAuthenticationFailed if credentials are invalid.
	Authenticate(ctx context.Context, req *Request) (*Identity, error)

	// SupportedMethods returns the authentication methods this authenticator handles.
	SupportedMethods() []AuthMethod
}
