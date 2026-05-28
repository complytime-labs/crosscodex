package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// GenerateCA creates a self-signed ECDSA P-256 certificate authority.
// The CA certificate has IsCA=true and can sign leaf certificates via GenerateCert.
func GenerateCA(opts ...Option) (*CertKeyPair, error) {
	o := defaultOptions()
	for _, fn := range opts {
		fn(&o)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}

	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{o.organization},
			CommonName:   o.organization + " CA",
		},
		NotBefore:             now,
		NotAfter:              now.Add(o.validDuration),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}

	keyPEM, err := encodeECKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode CA private key: %w", err)
	}

	return &CertKeyPair{
		CertPEM: encodeCert(certDER),
		KeyPEM:  keyPEM,
		Cert:    cert,
	}, nil
}

// GenerateCert creates a leaf certificate signed by the given CA.
// The certificate includes DNS and IP SANs from the options.
// Extended key usage defaults to [ServerAuth, ClientAuth] but can be
// overridden with WithExtKeyUsage (e.g., ClientAuth-only for client certs).
func GenerateCert(ca *CertKeyPair, opts ...Option) (*CertKeyPair, error) {
	o := defaultOptions()
	for _, fn := range opts {
		fn(&o)
	}

	if ca == nil || ca.Cert == nil || ca.KeyPEM == nil {
		return nil, fmt.Errorf("CA certificate and key are required")
	}

	// Parse CA private key
	caKeyBlock, _ := pem.Decode(ca.KeyPEM)
	if caKeyBlock == nil {
		return nil, fmt.Errorf("CA key is not valid PEM")
	}
	caKey, err := x509.ParseECPrivateKey(caKeyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA private key: %w", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key: %w", err)
	}

	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}

	now := time.Now()

	// Determine CN from first DNS name or default
	cn := "leaf"
	if len(o.dnsNames) > 0 {
		cn = o.dnsNames[0]
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{o.organization},
			CommonName:   cn,
		},
		NotBefore:   now,
		NotAfter:    now.Add(o.validDuration),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: o.extKeyUsage,
		DNSNames:    o.dnsNames,
		IPAddresses: o.ips,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, ca.Cert, &key.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create leaf certificate: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse leaf certificate: %w", err)
	}

	keyPEM, err := encodeECKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode leaf private key: %w", err)
	}

	return &CertKeyPair{
		CertPEM: encodeCert(certDER),
		KeyPEM:  keyPEM,
		Cert:    cert,
	}, nil
}

// GenerateDevPKI creates a complete PKI bundle: self-signed CA, server certificate
// (with localhost SANs), and client certificate. If WithOutputDir is specified,
// PEM files are written to disk.
func GenerateDevPKI(opts ...Option) (*PKIBundle, error) {
	o := defaultOptions()
	for _, fn := range opts {
		fn(&o)
	}

	ca, err := GenerateCA(
		WithOrganization(o.organization),
		WithValidDuration(o.validDuration),
	)
	if err != nil {
		return nil, fmt.Errorf("generate dev CA: %w", err)
	}

	server, err := GenerateCert(ca,
		WithOrganization(o.organization),
		WithValidDuration(o.validDuration),
		WithDNSNames(o.dnsNames...),
		WithIPs(o.ips...),
	)
	if err != nil {
		return nil, fmt.Errorf("generate dev server cert: %w", err)
	}

	client, err := GenerateCert(ca,
		WithOrganization(o.organization),
		WithValidDuration(o.validDuration),
		WithDNSNames("client"),
		WithIPs(),                                       // no IP SANs for client cert
		WithExtKeyUsage(x509.ExtKeyUsageClientAuth),     // client cert: ClientAuth only
	)
	if err != nil {
		return nil, fmt.Errorf("generate dev client cert: %w", err)
	}

	bundle := &PKIBundle{
		CA:     ca,
		Server: server,
		Client: client,
	}

	if o.outputDir != "" {
		if err := writeBundle(bundle, o.outputDir); err != nil {
			return nil, err
		}
		bundle.Dir = o.outputDir
	}

	return bundle, nil
}

// writeBundle writes all PEM files to dir. Cert files get 0644; key files get 0600.
func writeBundle(bundle *PKIBundle, dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create PKI output dir: %w", err)
	}

	files := []struct {
		name string
		data []byte
		perm os.FileMode
	}{
		{"ca.pem", bundle.CA.CertPEM, 0644},
		{"ca-key.pem", bundle.CA.KeyPEM, 0600},
		{"server.pem", bundle.Server.CertPEM, 0644},
		{"server-key.pem", bundle.Server.KeyPEM, 0600},
		{"client.pem", bundle.Client.CertPEM, 0644},
		{"client-key.pem", bundle.Client.KeyPEM, 0600},
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

func encodeECKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal EC private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: der,
	}), nil
}
