package authn

import (
	"context"
	"fmt"
)

// GSSAPIAuthenticator is a placeholder for future Kerberos/GSSAPI support.
// It always returns ErrUnsupportedMethod.
type GSSAPIAuthenticator struct{}

// NewGSSAPIAuthenticator creates a GSSAPI authenticator stub.
func NewGSSAPIAuthenticator() *GSSAPIAuthenticator {
	return &GSSAPIAuthenticator{}
}

// Authenticate always returns ErrUnsupportedMethod.
// GSSAPI (Kerberos) authentication is not yet implemented.
func (a *GSSAPIAuthenticator) Authenticate(_ context.Context, _ *Request) (*Identity, error) {
	return nil, fmt.Errorf("GSSAPI (Kerberos) authentication is not yet implemented: %w", ErrUnsupportedMethod)
}

// SupportedMethods returns AuthMethodKerberos.
func (a *GSSAPIAuthenticator) SupportedMethods() []AuthMethod {
	return []AuthMethod{AuthMethodKerberos}
}
