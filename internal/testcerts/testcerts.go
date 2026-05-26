// Package testcerts generates ephemeral TLS certificates for integration tests.
//
// It produces an ECDSA P-256 certificate authority, a server certificate
// (localhost + 127.0.0.1 SANs), and a client certificate. All certs have
// 10-year validity.
//
// Usage:
//
//	pki, err := testcerts.Generate()
//	if err != nil {
//	    log.Fatal(err)
//	}
//	if err := pki.WriteToDir("/path/to/certs"); err != nil {
//	    log.Fatal(err)
//	}
package testcerts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PKI holds a complete certificate authority, server cert, and client cert.
type PKI struct {
	CACert     []byte // PEM-encoded CA certificate
	CAKey      []byte // PEM-encoded CA private key
	ServerCert []byte // PEM-encoded server certificate
	ServerKey  []byte // PEM-encoded server private key
	ClientCert []byte // PEM-encoded client certificate
	ClientKey  []byte // PEM-encoded client private key
}

// Generate creates a new PKI with ECDSA P-256 keys and 10-year validity.
func Generate() (*PKI, error) {
	notBefore := time.Now()
	notAfter := notBefore.Add(3650 * 24 * time.Hour)

	// --- CA ---
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}

	caSerial, err := serialNumber()
	if err != nil {
		return nil, err
	}

	caTemplate := &x509.Certificate{
		SerialNumber: caSerial,
		Subject: pkix.Name{
			CommonName: "CrossCodex Test CA",
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create CA cert: %w", err)
	}

	// --- Server cert ---
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate server key: %w", err)
	}

	srvSerial, err := serialNumber()
	if err != nil {
		return nil, err
	}

	srvTemplate := &x509.Certificate{
		SerialNumber: srvSerial,
		Subject: pkix.Name{
			CommonName: "localhost",
		},
		NotBefore:   notBefore,
		NotAfter:    notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"localhost"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}

	srvCertDER, err := x509.CreateCertificate(rand.Reader, srvTemplate, caTemplate, &srvKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create server cert: %w", err)
	}

	// --- Client cert ---
	cliKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate client key: %w", err)
	}

	cliSerial, err := serialNumber()
	if err != nil {
		return nil, err
	}

	cliTemplate := &x509.Certificate{
		SerialNumber: cliSerial,
		Subject: pkix.Name{
			CommonName: "crosscodex-test-client",
		},
		NotBefore:   notBefore,
		NotAfter:    notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	cliCertDER, err := x509.CreateCertificate(rand.Reader, cliTemplate, caTemplate, &cliKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create client cert: %w", err)
	}

	return &PKI{
		CACert:     encodeCert(caCertDER),
		CAKey:      encodeECKey(caKey),
		ServerCert: encodeCert(srvCertDER),
		ServerKey:  encodeECKey(srvKey),
		ClientCert: encodeCert(cliCertDER),
		ClientKey:  encodeECKey(cliKey),
	}, nil
}

// WriteToDir writes all 6 PEM files to the given directory.
// Creates the directory (including parents) if it does not exist.
// Cert files get 0644 permissions; key files get 0600.
func (p *PKI) WriteToDir(dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create cert dir: %w", err)
	}

	files := []struct {
		name string
		data []byte
		perm os.FileMode
	}{
		{"ca.pem", p.CACert, 0644},
		{"ca-key.pem", p.CAKey, 0600},
		{"server.pem", p.ServerCert, 0644},
		{"server-key.pem", p.ServerKey, 0600},
		{"client.pem", p.ClientCert, 0644},
		{"client-key.pem", p.ClientKey, 0600},
	}

	for _, f := range files {
		path := filepath.Join(dir, f.name)
		if err := os.WriteFile(path, f.data, f.perm); err != nil {
			return fmt.Errorf("write %s: %w", f.name, err)
		}
	}
	return nil
}

func serialNumber() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate serial number: %w", err)
	}
	return serial, nil
}

// certFiles lists the PEM files that must be present in a valid cert
// directory, in the order used for fingerprint computation.
var certFiles = []string{
	"ca.pem",
	"ca-key.pem",
	"server.pem",
	"server-key.pem",
	"client.pem",
	"client-key.pem",
}

// fingerprintFile is the name of the SHA-256 fingerprint written alongside
// the PEM files. It allows external tools to detect when on-disk certs have
// been regenerated without parsing X.509.
const fingerprintFile = ".fingerprint"

// VerifyDir validates that the certificates in dir are parseable, not
// expired, and form a valid CA trust chain (CA signs both server and
// client certs). Returns nil on success.
func VerifyDir(dir string) error {
	// Read CA cert
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		return fmt.Errorf("read ca.pem: %w", err)
	}
	caBlock, _ := pem.Decode(caPEM)
	if caBlock == nil {
		return fmt.Errorf("ca.pem: not valid PEM")
	}
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return fmt.Errorf("ca.pem: parse: %w", err)
	}
	if !caCert.IsCA {
		return fmt.Errorf("ca.pem: certificate is not a CA")
	}
	now := time.Now()
	if now.Before(caCert.NotBefore) || now.After(caCert.NotAfter) {
		return fmt.Errorf("ca.pem: certificate not valid at current time (notBefore=%s, notAfter=%s)",
			caCert.NotBefore.Format(time.RFC3339), caCert.NotAfter.Format(time.RFC3339))
	}

	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	// Verify server cert
	if err := verifyLeaf(dir, "server.pem", pool, x509.ExtKeyUsageServerAuth); err != nil {
		return err
	}

	// Verify client cert
	if err := verifyLeaf(dir, "client.pem", pool, x509.ExtKeyUsageClientAuth); err != nil {
		return err
	}

	// Verify keys are parseable
	for _, keyFile := range []string{"ca-key.pem", "server-key.pem", "client-key.pem"} {
		keyPEM, err := os.ReadFile(filepath.Join(dir, keyFile))
		if err != nil {
			return fmt.Errorf("read %s: %w", keyFile, err)
		}
		keyBlock, _ := pem.Decode(keyPEM)
		if keyBlock == nil {
			return fmt.Errorf("%s: not valid PEM", keyFile)
		}
		if _, err := x509.ParseECPrivateKey(keyBlock.Bytes); err != nil {
			return fmt.Errorf("%s: parse: %w", keyFile, err)
		}
	}

	return nil
}

// verifyLeaf reads a leaf certificate from dir/name, checks it is not
// expired, and verifies it against the given CA pool with the specified
// extended key usage.
func verifyLeaf(dir, name string, pool *x509.CertPool, usage x509.ExtKeyUsage) error {
	leafPEM, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	leafBlock, _ := pem.Decode(leafPEM)
	if leafBlock == nil {
		return fmt.Errorf("%s: not valid PEM", name)
	}
	leafCert, err := x509.ParseCertificate(leafBlock.Bytes)
	if err != nil {
		return fmt.Errorf("%s: parse: %w", name, err)
	}
	now := time.Now()
	if now.Before(leafCert.NotBefore) || now.After(leafCert.NotAfter) {
		return fmt.Errorf("%s: certificate not valid at current time (notBefore=%s, notAfter=%s)",
			name, leafCert.NotBefore.Format(time.RFC3339), leafCert.NotAfter.Format(time.RFC3339))
	}
	if _, err := leafCert.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{usage},
	}); err != nil {
		return fmt.Errorf("%s: CA chain verification failed: %w", name, err)
	}
	return nil
}

// WriteFingerprint computes a SHA-256 hash of all cert PEM files in dir
// and writes it to dir/.fingerprint. This allows tools to detect when
// certs have been regenerated without parsing X.509.
func WriteFingerprint(dir string) error {
	fp, err := ComputeFingerprint(dir)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, fingerprintFile), []byte(fp+"\n"), 0644)
}

// ComputeFingerprint returns a SHA-256 hex digest computed over the
// concatenated contents of all PEM files in dir (in deterministic order).
func ComputeFingerprint(dir string) (string, error) {
	h := sha256.New()
	for _, name := range certFiles {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", fmt.Errorf("fingerprint: read %s: %w", name, err)
		}
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ReadFingerprint reads the stored fingerprint from dir/.fingerprint.
// Returns empty string and nil error if the file does not exist.
func ReadFingerprint(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, fingerprintFile))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read fingerprint: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

func encodeCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: der,
	})
}

func encodeECKey(key *ecdsa.PrivateKey) []byte {
	der, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: der,
	})
}
