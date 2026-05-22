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
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
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
