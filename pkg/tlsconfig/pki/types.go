package pki

import (
	"crypto/x509"
	"net"
	"time"
)

// defaultExtKeyUsage is used when no WithExtKeyUsage option is provided.
var defaultExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}

// CertKeyPair holds a PEM-encoded certificate and private key, along with the
// parsed x509.Certificate for programmatic inspection.
type CertKeyPair struct {
	CertPEM []byte            // PEM-encoded certificate
	KeyPEM  []byte            // PEM-encoded private key
	Cert    *x509.Certificate // Parsed certificate (nil until generation completes)
}

// PKIBundle is a complete development PKI: CA, server, and client certificates.
type PKIBundle struct {
	CA     *CertKeyPair // Self-signed certificate authority
	Server *CertKeyPair // Server certificate (signed by CA)
	Client *CertKeyPair // Client certificate (signed by CA)
	Dir    string       // Directory where PEM files were written (empty if in-memory only)
}

// Option configures certificate generation.
type Option func(*options)

type options struct {
	organization  string
	validDuration time.Duration
	dnsNames      []string
	ips           []net.IP
	extKeyUsage   []x509.ExtKeyUsage
	outputDir     string
}

func defaultOptions() options {
	return options{
		organization:  "CrossCodex Dev",
		validDuration: 365 * 24 * time.Hour, // 1 year
		dnsNames:      []string{"localhost"},
		ips:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		extKeyUsage:   defaultExtKeyUsage,
	}
}

// WithOrganization sets the certificate organization field.
// Default: "CrossCodex Dev".
func WithOrganization(org string) Option {
	return func(o *options) { o.organization = org }
}

// WithValidDuration sets how long generated certificates remain valid.
// Default: 1 year.
func WithValidDuration(d time.Duration) Option {
	return func(o *options) { o.validDuration = d }
}

// WithDNSNames sets the DNS Subject Alternative Names on leaf certificates.
// Default: ["localhost"].
func WithDNSNames(names ...string) Option {
	return func(o *options) { o.dnsNames = names }
}

// WithIPs sets the IP Subject Alternative Names on leaf certificates.
// Default: [127.0.0.1, ::1].
func WithIPs(ips ...net.IP) Option {
	return func(o *options) { o.ips = ips }
}

// WithExtKeyUsage sets the extended key usage on leaf certificates.
// Default: [ServerAuth, ClientAuth].
func WithExtKeyUsage(usages ...x509.ExtKeyUsage) Option {
	return func(o *options) { o.extKeyUsage = usages }
}

// WithOutputDir causes generated PEM files to be written to the given directory.
// The directory is created if it does not exist. Default: "" (in-memory only).
func WithOutputDir(dir string) Option {
	return func(o *options) { o.outputDir = dir }
}
