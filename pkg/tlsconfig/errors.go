package tlsconfig

import "errors"

var (
	// ErrFIPSNotEnabled indicates FIPS mode was requested but the binary was
	// not built with GOEXPERIMENT=boringcrypto. Rebuild with: task build FIPS=1
	ErrFIPSNotEnabled = errors.New("FIPS mode requested but binary not built with GOEXPERIMENT=boringcrypto; rebuild with: task build FIPS=1")

	// ErrNoCiphersAvailable indicates all cipher suites were excluded by the
	// combined FIPS, cipher_allow, and cipher_deny filters.
	ErrNoCiphersAvailable = errors.New("no cipher suites remaining after filtering")

	// ErrMissingCert indicates a TLS certificate file path is required but not set.
	ErrMissingCert = errors.New("TLS certificate file required but not specified")

	// ErrMissingKey indicates a TLS private key file path is required but not set.
	ErrMissingKey = errors.New("TLS key file required but not specified")

	// ErrMissingCA indicates a CA certificate file path is required but not set.
	ErrMissingCA = errors.New("CA certificate file required but not specified")

	// ErrInvalidMode indicates the TLS mode is not one of off, server-only, mutual.
	ErrInvalidMode = errors.New("unknown TLS mode")

	// ErrInvalidCertificate indicates a certificate file could not be parsed.
	ErrInvalidCertificate = errors.New("invalid certificate")

	// ErrCertificateLoadFailed indicates certificate loading from disk failed.
	ErrCertificateLoadFailed = errors.New("failed to load certificate")
)
