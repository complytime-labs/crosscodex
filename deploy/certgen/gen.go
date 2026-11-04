//go:build ignore

// Command gen generates and verifies the production TLS PKI for the compose
// deployment. One shared server cert (SANs cover every compose service name)
// plus one client cert plus the CA, written as PEM files with a fingerprint,
// plus the in-toto attestation signing keypair.
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
// chain, key/cert matches, the compose SANs below, the fingerprint, at least
// a day of validity left, and a matching attestation keypair.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
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
			VerifyExtra:        verifyAttestationKeypair,
		},
		Generate:   generate,
		WriteExtra: writeAttestationKeypair,
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

// writeAttestationKeypair generates the ECDSA P-256 in-toto attestation
// signing keypair the pipeline role requires and writes it alongside the TLS
// PKI. attestation-key.pem holds the SEC1 "EC PRIVATE KEY" that
// attestation.FileKeyProvider parses with x509.ParseECPrivateKey;
// attestation-pub.pem holds the PKIX "PUBLIC KEY" it parses with
// x509.ParsePKIXPublicKey. The "-key.pem" suffix makes deploy:certs:load apply
// the same group-read-only 0640 mode it uses for the TLS private keys. These
// keys are deliberately outside testcerts' fingerprint: they are only ever
// written together with a new CA (which changes the fingerprint and forces a
// reload), and verifyAttestationKeypair guards their presence and pairing.
func writeAttestationKeypair(dir string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate attestation key: %w", err)
	}

	privDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal attestation private key: %w", err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privDER})
	if err := os.WriteFile(filepath.Join(dir, "attestation-key.pem"), privPEM, 0o600); err != nil {
		return fmt.Errorf("write attestation private key: %w", err)
	}

	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return fmt.Errorf("marshal attestation public key: %w", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	if err := os.WriteFile(filepath.Join(dir, "attestation-pub.pem"), pubPEM, 0o644); err != nil {
		return fmt.Errorf("write attestation public key: %w", err)
	}

	return nil
}

// verifyAttestationKeypair checks both attestation files parse the way
// attestation.FileKeyProvider parses them and hold the same key.
func verifyAttestationKeypair(dir string) error {
	privPEM, err := os.ReadFile(filepath.Join(dir, "attestation-key.pem"))
	if err != nil {
		return fmt.Errorf("read attestation-key.pem: %w", err)
	}
	privBlock, _ := pem.Decode(privPEM)
	if privBlock == nil {
		return fmt.Errorf("attestation-key.pem: not valid PEM")
	}
	key, err := x509.ParseECPrivateKey(privBlock.Bytes)
	if err != nil {
		return fmt.Errorf("attestation-key.pem: parse: %w", err)
	}

	pubPEM, err := os.ReadFile(filepath.Join(dir, "attestation-pub.pem"))
	if err != nil {
		return fmt.Errorf("read attestation-pub.pem: %w", err)
	}
	pubBlock, _ := pem.Decode(pubPEM)
	if pubBlock == nil {
		return fmt.Errorf("attestation-pub.pem: not valid PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(pubBlock.Bytes)
	if err != nil {
		return fmt.Errorf("attestation-pub.pem: parse: %w", err)
	}
	if !key.PublicKey.Equal(pub) {
		return fmt.Errorf("attestation-pub.pem does not match attestation-key.pem")
	}
	return nil
}
