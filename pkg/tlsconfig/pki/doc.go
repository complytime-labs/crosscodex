// Package pki provides ECDSA P-256 certificate generation for development and testing.
//
// GenerateDevPKI creates a complete PKI (CA, server cert, client cert) suitable
// for local development and integration tests. For production, use certificates
// issued by your organization's CA.
//
// Example usage:
//
//	bundle, err := pki.GenerateDevPKI(
//	    pki.WithDNSNames("localhost", "myservice.local"),
//	    pki.WithOutputDir("/tmp/dev-certs"),
//	)
//	if err != nil {
//	    return err
//	}
//	// bundle.CA, bundle.Server, bundle.Client contain PEM-encoded certs and keys
package pki
