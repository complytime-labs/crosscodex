package authn

import "errors"

var (
	// ErrAuthenticationFailed indicates authentication failed.
	ErrAuthenticationFailed = errors.New("authentication failed")

	// ErrUnsupportedMethod indicates the authentication method is not supported.
	ErrUnsupportedMethod = errors.New("unsupported authentication method")

	// ErrInsufficientRole is returned when an identity lacks all of the required roles.
	ErrInsufficientRole = errors.New("insufficient role")
)
