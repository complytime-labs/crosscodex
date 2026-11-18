package testcerts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// AttestationKeyFile is the attestation signing private key's file name.
	// The "-key.pem" suffix makes deploy:certs:load apply the same
	// group-read-only 0640 mode it uses for the TLS private keys.
	AttestationKeyFile = "attestation-key.pem"
	// AttestationPubFile is the attestation verification public key's file name.
	AttestationPubFile = "attestation-pub.pem"
)

// WriteAttestationKeypair generates the ECDSA P-256 in-toto attestation
// signing keypair the pipeline role requires and writes it into dir, for use
// as EnsureOptions.WriteExtra. AttestationKeyFile holds the SEC1 "EC PRIVATE
// KEY" that attestation.FileKeyProvider parses with x509.ParseECPrivateKey;
// AttestationPubFile holds the PKIX "PUBLIC KEY" it parses with
// x509.ParsePKIXPublicKey. These keys are deliberately outside the
// fingerprint: they are only ever written together with a new CA (which
// changes the fingerprint and forces a reload), and VerifyAttestationKeypair
// guards their presence and pairing.
func WriteAttestationKeypair(dir string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate attestation key: %w", err)
	}

	privDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal attestation private key: %w", err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privDER})
	if err := os.WriteFile(filepath.Join(dir, AttestationKeyFile), privPEM, 0o600); err != nil {
		return fmt.Errorf("write attestation private key: %w", err)
	}

	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return fmt.Errorf("marshal attestation public key: %w", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	if err := os.WriteFile(filepath.Join(dir, AttestationPubFile), pubPEM, 0o644); err != nil {
		return fmt.Errorf("write attestation public key: %w", err)
	}

	return nil
}

// VerifyAttestationKeypair checks both attestation files in dir parse the way
// attestation.FileKeyProvider parses them and hold the same key, for use as
// VerifyOptions.VerifyExtra.
func VerifyAttestationKeypair(dir string) error {
	privPEM, err := os.ReadFile(filepath.Join(dir, AttestationKeyFile))
	if err != nil {
		return fmt.Errorf("read %s: %w", AttestationKeyFile, err)
	}
	privBlock, _ := pem.Decode(privPEM)
	if privBlock == nil {
		return fmt.Errorf("%s: not valid PEM", AttestationKeyFile)
	}
	key, err := x509.ParseECPrivateKey(privBlock.Bytes)
	if err != nil {
		return fmt.Errorf("%s: parse: %w", AttestationKeyFile, err)
	}

	pubPEM, err := os.ReadFile(filepath.Join(dir, AttestationPubFile))
	if err != nil {
		return fmt.Errorf("read %s: %w", AttestationPubFile, err)
	}
	pubBlock, _ := pem.Decode(pubPEM)
	if pubBlock == nil {
		return fmt.Errorf("%s: not valid PEM", AttestationPubFile)
	}
	pub, err := x509.ParsePKIXPublicKey(pubBlock.Bytes)
	if err != nil {
		return fmt.Errorf("%s: parse: %w", AttestationPubFile, err)
	}
	if !key.PublicKey.Equal(pub) {
		return fmt.Errorf("%s does not match %s", AttestationPubFile, AttestationKeyFile)
	}
	return nil
}
