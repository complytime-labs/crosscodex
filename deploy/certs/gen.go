//go:build ignore

// Command gen generates and verifies the production TLS PKI for the compose
// deployment. One shared server cert (SANs cover every compose service name)
// plus one client cert plus the CA, written as PEM files with a fingerprint.
//
// Usage:
//
//	go run ./deploy/certs/gen.go <output-dir>
//	go run ./deploy/certs/gen.go --verify <output-dir>
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

func main() {
	if len(os.Args) == 3 && os.Args[1] == "--verify" {
		if err := testcerts.VerifyDir(os.Args[2]); err != nil {
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

	bundle, err := pki.GenerateDevPKI(
		pki.WithOrganization("CrossCodex"),
		pki.WithValidDuration(3650*24*time.Hour),
		pki.WithDNSNames("localhost", "db", "nats", "litellm", "crosscodexd"),
		pki.WithIPs(net.IPv4(127, 0, 0, 1)),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate PKI: %v\n", err)
		os.Exit(1)
	}

	p := &testcerts.PKI{
		CACert:     bundle.CA.CertPEM,
		CAKey:      bundle.CA.KeyPEM,
		ServerCert: bundle.Server.CertPEM,
		ServerKey:  bundle.Server.KeyPEM,
		ClientCert: bundle.Client.CertPEM,
		ClientKey:  bundle.Client.KeyPEM,
	}
	if err := p.WriteToDir(dir); err != nil {
		fmt.Fprintf(os.Stderr, "write certs: %v\n", err)
		os.Exit(1)
	}
	if err := testcerts.WriteFingerprint(dir); err != nil {
		fmt.Fprintf(os.Stderr, "write fingerprint: %v\n", err)
		os.Exit(1)
	}
	if err := writeAttestationKeypair(dir); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Certificates written to %s\n", dir)
}

// writeAttestationKeypair generates the ECDSA P-256 in-toto attestation
// signing keypair the pipeline role requires and writes it alongside the TLS
// PKI. attestation-key.pem holds the SEC1 "EC PRIVATE KEY" that
// attestation.FileKeyProvider parses with x509.ParseECPrivateKey;
// attestation-pub.pem holds the PKIX "PUBLIC KEY" it parses with
// x509.ParsePKIXPublicKey. The "-key.pem" suffix makes deploy:certs:load apply
// the same group-read-only 0640 mode it uses for the TLS private keys. These
// keys are deliberately outside testcerts' fingerprint: they ride along on a
// full regeneration (a new CA changes the fingerprint and forces a reload),
// and their absence is guarded separately by the deploy:certs:generate status.
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
