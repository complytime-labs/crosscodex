package authn

import "errors"

var (
	// ErrAuthenticationFailed indicates authentication failed.
	ErrAuthenticationFailed = errors.New("authentication failed")

	// ErrUnsupportedMethod indicates the authentication method is not supported.
	ErrUnsupportedMethod = errors.New("unsupported authentication method")

	// ErrInvalidCredentials indicates the credentials are malformed.
	ErrInvalidCredentials = errors.New("invalid credentials")

	// ErrTenantNotFound indicates the tenant does not exist.
	ErrTenantNotFound = errors.New("tenant not found")
)
