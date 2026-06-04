package tlsconfig

import (
	"crypto/tls"

	"go.opentelemetry.io/otel/metric"

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

// MakeGetCertificateWithMeter exposes makeGetCertificateWithMeter for external tests.
func MakeGetCertificateWithMeter(certFile, keyFile string, m metric.Meter) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return makeGetCertificateWithMeter(certFile, keyFile, m)
}

// MakeGetClientCertificate exposes makeGetClientCertificate for external tests.
// Returns the concrete callback type for compile-time safety.
func MakeGetClientCertificate(certFile, keyFile string) func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return makeGetClientCertificate(certFile, keyFile)
}

// MakeGetClientCertificateWithMeter exposes makeGetClientCertificateWithMeter for external tests.
func MakeGetClientCertificateWithMeter(certFile, keyFile string, m metric.Meter) func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return makeGetClientCertificateWithMeter(certFile, keyFile, m)
}

// TelemetryFields exposes telemetry wiring state for BDD assertions.
type TelemetryFields struct {
	HasTracer bool
	HasMeter  bool
}

// ExportTelemetryFields returns the telemetry wiring state of a Resolver.
func ExportTelemetryFields(r Resolver) TelemetryFields {
	return TelemetryFields{
		HasTracer: r.Tracer != nil,
		HasMeter:  r.Meter != nil,
	}
}
