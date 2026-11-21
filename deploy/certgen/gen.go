//go:build ignore

// Command gen generates and verifies the production TLS PKI for the compose
// deployment. One shared server cert (SANs cover every compose service name,
// serverAuth only) plus one client cert (clientAuth only) plus the CA, written
// as PEM files with a fingerprint, plus the in-toto attestation signing
// keypair.
//
// Usage:
//
//	go run ./deploy/certgen/gen.go <output-dir>
//	go run ./deploy/certgen/gen.go --verify <output-dir>
//
// Without --verify, keeps a set that already verifies and otherwise replaces
// the whole directory atomically with a new one (see testcerts.Ensure), so
// concurrent and interrupted runs never leave a mixed or partial set.
// <output-dir> must be dedicated to this material: regeneration discards
// anything else in it. A regeneration mints a new CA and attestation key.
//
// With --verify, checks everything Ensure requires to keep a set: the TLS
// chain, key/cert matches, the compose SANs below, a serverAuth-only server
// cert, the fingerprint, at least a day of validity left, and a matching
// attestation keypair.
package main

import (
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/complytime-labs/crosscodex/internal/testcerts"
	pki "github.com/complytime-labs/crosscodex/pkg/tlsconfig/pki"
)

// dnsNames are the compose service names the shared server cert must cover.
// Adding one here makes --verify reject older sets, so the next
// deploy:certs:generate replaces them.
var dnsNames = []string{"localhost", "db", "nats", "litellm", "praxis", "crosscodexd"}

var ips = []net.IP{net.IPv4(127, 0, 0, 1)}

func main() {
	opts := testcerts.EnsureOptions{
		Verify: testcerts.VerifyOptions{
			DNSNames:           dnsNames,
			IPs:                ips,
			MinRemaining:       24 * time.Hour,
			RequireFingerprint: true,
			// db, nats and the LLM gateway hold server-key.pem; a server cert
			// that also permits clientAuth would let any of them connect to
			// nats or db as a CA-trusted client. Older dual-use sets fail
			// here, so deploy:certs:generate replaces them.
			ServerAuthOnly: true,
			VerifyExtra:    testcerts.VerifyAttestationKeypair,
		},
		Generate:   generate,
		WriteExtra: testcerts.WriteAttestationKeypair,
	}

	if len(os.Args) == 3 && os.Args[1] == "--verify" {
		if err := testcerts.VerifyDirWith(os.Args[2], opts.Verify); err != nil {
			fmt.Fprintf(os.Stderr, "verify certs: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Certificates in %s are valid\n", os.Args[2])
		return
	}
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: %s [--verify] <output-dir>\n", os.Args[0])
		os.Exit(1)
	}
	dir := os.Args[1]

	generated, err := testcerts.Ensure(dir, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ensure certs: %v\n", err)
		os.Exit(1)
	}
	if !generated {
		fmt.Printf("Certificates in %s are valid; kept\n", dir)
		return
	}
	fmt.Printf("Certificates written to %s\n", dir)
}

func generate() (*testcerts.PKI, error) {
	bundle, err := pki.GenerateDevPKI(
		pki.WithOrganization("CrossCodex"),
		pki.WithValidDuration(3650*24*time.Hour),
		pki.WithDNSNames(dnsNames...),
		pki.WithIPs(ips...),
		pki.WithExtKeyUsage(x509.ExtKeyUsageServerAuth),
	)
	if err != nil {
		return nil, fmt.Errorf("generate PKI: %w", err)
	}
	return &testcerts.PKI{
		CACert:     bundle.CA.CertPEM,
		CAKey:      bundle.CA.KeyPEM,
		ServerCert: bundle.Server.CertPEM,
		ServerKey:  bundle.Server.KeyPEM,
		ClientCert: bundle.Client.CertPEM,
		ClientKey:  bundle.Client.KeyPEM,
	}, nil
}
