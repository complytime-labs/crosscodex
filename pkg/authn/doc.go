// Package authn provides multi-method authentication for CrossCodex services.
//
// Each authentication method (X.509 mTLS, Kerberos, SAML) is a standalone
// Authenticator implementation. A Registry holds authenticators in order
// and dispatches requests to each until one succeeds or returns a fatal error.
//
// X.509 is fully implemented. GSSAPI (Kerberos) and SAML are stubbed with
// ErrUnsupportedMethod for future implementation.
//
// Example usage:
//
//	x509Auth, err := authn.NewX509Authenticator(authn.X509Config{
//	    SingleTenant:  true,
//	    DefaultTenant: "default",
//	})
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	registry, err := authn.NewRegistry(auditEmitter, []authn.Authenticator{x509Auth})
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	identity, err := registry.Authenticate(ctx, &authn.Request{
//	    TLSState: conn.ConnectionState(),
//	    ClientIP: remoteAddr,
//	})
package authn
