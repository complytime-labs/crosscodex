package tlsconfig

import (
	"crypto/tls"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

// Export unexported functions for BDD tests in the tlsconfig_test package.
// This follows the Go standard library convention (e.g., export_test.go).

// MergeConfigFields returns the individual fields of a resolvedConfig for testing.
func MergeConfigFields(cfg config.TLSConfig, target string) (mode, ca, cert, key string) {
	rc := mergeConfig(cfg, target)
	return rc.mode, rc.ca, rc.cert, rc.key
}

// FipsCipherSuites exposes fipsCipherSuites for external tests.
var FipsCipherSuites = fipsCipherSuites

// FilterCiphers exposes filterCiphers for external tests.
var FilterCiphers = filterCiphers

// AllNonInsecureCipherIDs exposes allNonInsecureCipherIDs for external tests.
var AllNonInsecureCipherIDs = allNonInsecureCipherIDs

// MakeGetCertificate exposes makeGetCertificate for external tests.
// Returns the concrete callback type for compile-time safety.
func MakeGetCertificate(certFile, keyFile string) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return makeGetCertificate(certFile, keyFile)
}

// MakeGetClientCertificate exposes makeGetClientCertificate for external tests.
// Returns the concrete callback type for compile-time safety.
func MakeGetClientCertificate(certFile, keyFile string) func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return makeGetClientCertificate(certFile, keyFile)
}
