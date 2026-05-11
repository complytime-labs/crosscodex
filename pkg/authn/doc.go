// Package authn provides multi-method authentication.
//
// Supports mTLS, Kerberos, and SAML authentication with extensible
// authenticator interface.
//
// Example usage:
//
//	auth := authn.NewMTLSAuthenticator(tlsConfig)
//	identity, err := auth.Authenticate(ctx, req)
//	if err != nil {
//	    return err
//	}
//	fmt.Printf("Authenticated: %s (tenant: %s)\n", identity.Subject, identity.TenantID)
package authn
