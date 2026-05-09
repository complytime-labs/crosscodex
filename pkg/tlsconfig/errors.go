package tlsconfig

import "errors"

var (
	// ErrFIPSNotEnabled indicates FIPS mode is required but not active.
	ErrFIPSNotEnabled = errors.New("FIPS mode not enabled")

	// ErrInvalidCertificate indicates the certificate is invalid or expired.
	ErrInvalidCertificate = errors.New("invalid certificate")

	// ErrCertificateLoadFailed indicates certificate loading failed.
	ErrCertificateLoadFailed = errors.New("failed to load certificate")

	// ErrWeakCipherSuite indicates a non-FIPS-approved cipher suite was used.
	ErrWeakCipherSuite = errors.New("weak cipher suite not allowed in FIPS mode")
)
