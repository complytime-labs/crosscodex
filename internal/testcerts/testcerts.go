// Package testcerts generates ephemeral TLS certificates for integration tests.
//
// It delegates certificate generation to pkg/tlsconfig/pki, keeping only
// the test-oriented PKI struct and file management utilities (WriteToDir,
// VerifyDir, fingerprinting).
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
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	pkigen "github.com/complytime-labs/crosscodex/pkg/tlsconfig/pki"
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
	bundle, err := pkigen.GenerateDevPKI(
		pkigen.WithOrganization("CrossCodex Test"),
		pkigen.WithValidDuration(3650*24*time.Hour),
		// Include compose service names so containers can verify each other's
		// server certs when connecting by service name (e.g. db, nats).
		pkigen.WithDNSNames("localhost", "db", "nats", "storage", "authn-proxy", "grpc"),
		pkigen.WithIPs(net.IPv4(127, 0, 0, 1)),
	)
	if err != nil {
		return nil, fmt.Errorf("generate test PKI: %w", err)
	}

	return &PKI{
		CACert:     bundle.CA.CertPEM,
		CAKey:      bundle.CA.KeyPEM,
		ServerCert: bundle.Server.CertPEM,
		ServerKey:  bundle.Server.KeyPEM,
		ClientCert: bundle.Client.CertPEM,
		ClientKey:  bundle.Client.KeyPEM,
	}, nil
}

// WriteToDir writes all 6 PEM files to the given directory.
// Creates the directory (including parents) if it does not exist.
// Cert files get 0644 permissions; key files get 0600.
func (p *PKI) WriteToDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create cert dir: %w", err)
	}

	files := []struct {
		name string
		data []byte
		perm os.FileMode
	}{
		{"ca.pem", p.CACert, 0o644},
		{"ca-key.pem", p.CAKey, 0o600},
		{"server.pem", p.ServerCert, 0o644},
		{"server-key.pem", p.ServerKey, 0o600},
		{"client.pem", p.ClientCert, 0o644},
		{"client-key.pem", p.ClientKey, 0o600},
	}

	for _, f := range files {
		path := filepath.Join(dir, f.name)
		if err := os.WriteFile(path, f.data, f.perm); err != nil {
			return fmt.Errorf("write %s: %w", f.name, err)
		}
	}
	return nil
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

// VerifyOptions states what a cert directory must satisfy beyond the
// baseline VerifyDir checks. The zero value adds nothing.
type VerifyOptions struct {
	// DNSNames and IPs must each be covered by the server certificate's SANs.
	DNSNames []string
	IPs      []net.IP
	// MinRemaining is how long every certificate must remain valid from now,
	// so a set that is about to expire is replaced before a run, not during it.
	MinRemaining time.Duration
	// RequireFingerprint requires .fingerprint to exist and match the PEM files.
	RequireFingerprint bool
	// VerifyExtra checks caller-owned files in the directory (e.g. the deploy
	// attestation keypair). Nil skips it.
	VerifyExtra func(dir string) error
}

// VerifyDir validates the baseline every cert directory must meet: all six
// PEM files parse, every certificate is valid now, the CA signs the server
// (serverAuth) and client (clientAuth) certificates, and each private key
// matches its certificate. Returns nil on success.
func VerifyDir(dir string) error {
	return VerifyDirWith(dir, VerifyOptions{})
}

// VerifyDirWith runs the VerifyDir checks plus the requirements in opts.
// The error names the offending file and what is wrong with it.
func VerifyDirWith(dir string, opts VerifyOptions) error {
	if err := verifyDir(dir, opts); err != nil {
		return fmt.Errorf("cert dir %s is unusable: %w; regenerate it (the cert generator replaces the whole set when verification fails)", dir, err)
	}
	return nil
}

func verifyDir(dir string, opts VerifyOptions) error {
	now := time.Now()

	caCert, err := readCert(dir, "ca.pem")
	if err != nil {
		return err
	}
	if !caCert.IsCA {
		return fmt.Errorf("ca.pem: certificate is not a CA")
	}
	if err := checkValidity("ca.pem", caCert, now, opts.MinRemaining); err != nil {
		return err
	}
	if err := checkKeyMatches(dir, "ca-key.pem", "ca.pem", caCert); err != nil {
		return err
	}

	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	serverCert, err := verifyLeaf(dir, "server.pem", "server-key.pem", pool, x509.ExtKeyUsageServerAuth, now, opts.MinRemaining)
	if err != nil {
		return err
	}
	if _, err := verifyLeaf(dir, "client.pem", "client-key.pem", pool, x509.ExtKeyUsageClientAuth, now, opts.MinRemaining); err != nil {
		return err
	}

	for _, name := range opts.DNSNames {
		if err := serverCert.VerifyHostname(name); err != nil {
			return fmt.Errorf("server.pem does not cover required name %q", name)
		}
	}
	for _, ip := range opts.IPs {
		if err := serverCert.VerifyHostname(ip.String()); err != nil {
			return fmt.Errorf("server.pem does not cover required IP %q", ip.String())
		}
	}

	if opts.RequireFingerprint {
		stored, err := ReadFingerprint(dir)
		if err != nil {
			return err
		}
		if stored == "" {
			return fmt.Errorf("%s is missing", fingerprintFile)
		}
		computed, err := ComputeFingerprint(dir)
		if err != nil {
			return err
		}
		if stored != computed {
			return fmt.Errorf("%s does not match the PEM files (the set was modified after generation)", fingerprintFile)
		}
	}

	if opts.VerifyExtra != nil {
		if err := opts.VerifyExtra(dir); err != nil {
			return err
		}
	}
	return nil
}

// verifyLeaf checks the leaf certificate dir/name is within its validity
// window, chains to pool with the given extended key usage, and matches the
// private key in dir/keyName. Returns the parsed certificate.
func verifyLeaf(dir, name, keyName string, pool *x509.CertPool, usage x509.ExtKeyUsage, now time.Time, minRemaining time.Duration) (*x509.Certificate, error) {
	leafCert, err := readCert(dir, name)
	if err != nil {
		return nil, err
	}
	if err := checkValidity(name, leafCert, now, minRemaining); err != nil {
		return nil, err
	}
	if _, err := leafCert.Verify(x509.VerifyOptions{
		Roots:       pool,
		KeyUsages:   []x509.ExtKeyUsage{usage},
		CurrentTime: now,
	}); err != nil {
		return nil, fmt.Errorf("%s: CA chain verification failed: %w", name, err)
	}
	if err := checkKeyMatches(dir, keyName, name, leafCert); err != nil {
		return nil, err
	}
	return leafCert, nil
}

func readCert(dir, name string) (*x509.Certificate, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s: not valid PEM", name)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: parse: %w", name, err)
	}
	return cert, nil
}

// checkValidity rejects a certificate outside its validity window at now, or
// one whose notAfter falls within minRemaining of now.
func checkValidity(name string, cert *x509.Certificate, now time.Time, minRemaining time.Duration) error {
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		return fmt.Errorf("%s: certificate not valid at current time (notBefore=%s, notAfter=%s)",
			name, cert.NotBefore.Format(time.RFC3339), cert.NotAfter.Format(time.RFC3339))
	}
	if minRemaining > 0 && cert.NotAfter.Before(now.Add(minRemaining)) {
		return fmt.Errorf("%s: certificate expires within %s (notAfter=%s)",
			name, minRemaining, cert.NotAfter.Format(time.RFC3339))
	}
	return nil
}

// checkKeyMatches parses the EC private key in dir/keyName and checks its
// public half is the one certified in cert (read from certName).
func checkKeyMatches(dir, keyName, certName string, cert *x509.Certificate) error {
	data, err := os.ReadFile(filepath.Join(dir, keyName))
	if err != nil {
		return fmt.Errorf("read %s: %w", keyName, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return fmt.Errorf("%s: not valid PEM", keyName)
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("%s: parse: %w", keyName, err)
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return fmt.Errorf("%s does not match %s (mixed set from different generator runs)", keyName, certName)
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
	return os.WriteFile(filepath.Join(dir, fingerprintFile), []byte(fp+"\n"), 0o644)
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
