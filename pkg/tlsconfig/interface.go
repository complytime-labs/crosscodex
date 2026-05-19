package tlsconfig

import (
	"context"
	"crypto/tls"
)

// Builder creates tls.Config with FIPS enforcement.
//
// Implementations must use FIPS-approved cipher suites and
// minimum TLS 1.2 when FIPS mode is enabled.
type Builder interface {
	// BuildServer creates a server TLS configuration.
	// Requires client certificate verification if clientCA is provided.
	BuildServer(ctx context.Context, certFile, keyFile, clientCA string) (*tls.Config, error)

	// BuildClient creates a client TLS configuration.
	// Uses client certificates if certFile and keyFile are provided.
	BuildClient(ctx context.Context, certFile, keyFile, serverCA string) (*tls.Config, error)

	// ValidateFIPS checks that the system is running in FIPS mode.
	// Returns an error if FIPS mode is required but not active.
	ValidateFIPS(ctx context.Context) error

	// GetFIPSStatus returns the current FIPS mode status.
	GetFIPSStatus(ctx context.Context) (*FIPSStatus, error)

	// ParseCertificate loads and parses a certificate file.
	ParseCertificate(ctx context.Context, certFile string) (*CertificateInfo, error)
}
