package authn

import (
	"context"
	"fmt"
)

// SAMLAuthenticator is a placeholder for future SAML support.
// It always returns ErrUnsupportedMethod.
type SAMLAuthenticator struct{}

// NewSAMLAuthenticator creates a SAML authenticator stub.
func NewSAMLAuthenticator() *SAMLAuthenticator {
	return &SAMLAuthenticator{}
}

// Authenticate always returns ErrUnsupportedMethod.
// SAML authentication is not yet implemented.
func (a *SAMLAuthenticator) Authenticate(_ context.Context, _ *Request) (*Identity, error) {
	return nil, fmt.Errorf("SAML authentication is not yet implemented: %w", ErrUnsupportedMethod)
}

// SupportedMethods returns AuthMethodSAML.
func (a *SAMLAuthenticator) SupportedMethods() []AuthMethod {
	return []AuthMethod{AuthMethodSAML}
}
