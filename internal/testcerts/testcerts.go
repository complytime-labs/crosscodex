// Package testcerts generates ephemeral TLS certificates for integration tests.
//
// It delegates certificate generation to pkg/tlsconfig/pki, keeping only
// the test-oriented PKI struct and file management utilities (WriteToDir,
// VerifyDir, VerifyDirWith, fingerprinting) and Ensure, which keeps a cert
// directory valid under concurrent and interrupted generator runs.
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
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	pkigen "github.com/complytime-labs/crosscodex/pkg/tlsconfig/pki"
)

// PKI holds a complete certificate authority, a client cert, and either one
// server cert shared by every server (ServerCert, ServerKey: tests, the
// embedded daemon) or one per service (Services: the deploy stack).
type PKI struct {
	CACert     []byte // PEM-encoded CA certificate
	CAKey      []byte // PEM-encoded CA private key
	ServerCert []byte // PEM-encoded shared server certificate; nil when Services is used
	ServerKey  []byte // PEM-encoded shared server private key; nil when Services is used
	ClientCert []byte // PEM-encoded client certificate
	ClientKey  []byte // PEM-encoded client private key
	// Services holds each service's own server certificate and key, by service
	// name. A service holding only its own key cannot impersonate another.
	Services map[string]ServiceCert
}

// ServiceCert is one service's PEM-encoded server certificate and private key.
type ServiceCert struct {
	Cert []byte
	Key  []byte
}

// ServiceIdentity names one TLS server and the names its clients dial. Its
// certificate carries exactly these SANs and nothing else, so it is rejected
// for any other service's hostname.
type ServiceIdentity struct {
	// Name is the file stem: the cert is <Name>.pem and the key <Name>-key.pem.
	// Lowercase letters and digits only, and not one of reservedServiceNames.
	Name     string
	DNSNames []string
	IPs      []net.IP
}

// serviceNamePattern keeps a service name usable as a file stem: no path
// separators, no dots, and no hyphen (so <a>-key.pem never collides with
// another service's <b>.pem).
var serviceNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// reservedServiceNames would overwrite a file the set already uses: the CA, the
// client identity, the shared server cert of earlier releases, and the
// attestation keypair (attestation-key.pem).
var reservedServiceNames = []string{"ca", "client", "server", "attestation"}

// ErrSharedServerKey reports a cert directory in the layout of earlier
// releases, where every TLS service shared one server key.
var ErrSharedServerKey = errors.New("cert dir uses the shared server key layout")

func validateServiceName(name string) error {
	if !serviceNamePattern.MatchString(name) {
		return fmt.Errorf("service name %q is invalid: use a lowercase letter followed by lowercase letters or digits", name)
	}
	if slices.Contains(reservedServiceNames, name) {
		return fmt.Errorf("service name %q is reserved: %s.pem or %s-key.pem would overwrite a file the set already uses", name, name, name)
	}
	return nil
}

// generateDNSNames are the server SANs Generate issues: localhost plus the
// compose service names, so containers can verify each other's server certs
// when connecting by service name (e.g. db, nats).
var generateDNSNames = []string{"localhost", "db", "nats", "storage", "authn-proxy", "grpc"}

// Generate creates a new PKI with ECDSA P-256 keys and 10-year validity. The
// server certificate is serverAuth-only: the client certificate is the only
// identity that can authenticate as a client.
func Generate() (*PKI, error) {
	bundle, err := pkigen.GenerateDevPKI(
		pkigen.WithOrganization("CrossCodex Test"),
		pkigen.WithValidDuration(3650*24*time.Hour),
		pkigen.WithDNSNames(generateDNSNames...),
		pkigen.WithIPs(net.IPv4(127, 0, 0, 1)),
		pkigen.WithExtKeyUsage(x509.ExtKeyUsageServerAuth),
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

// GenerateServices creates a PKI in which every service in services gets its
// own ECDSA P-256 key and a serverAuth-only certificate naming only that
// service, plus a clientAuth-only client certificate (CN client). No shared
// server certificate is issued, so a service that holds only its own key cannot
// impersonate another. It validates services before generating anything.
func GenerateServices(organization string, valid time.Duration, services []ServiceIdentity) (*PKI, error) {
	if len(services) == 0 {
		return nil, errors.New("generate service PKI: at least one service is required")
	}
	seen := map[string]bool{}
	for _, s := range services {
		if err := validateServiceName(s.Name); err != nil {
			return nil, fmt.Errorf("generate service PKI: %w", err)
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("generate service PKI: service %q is listed twice", s.Name)
		}
		seen[s.Name] = true
		if len(s.DNSNames) == 0 && len(s.IPs) == 0 {
			return nil, fmt.Errorf("generate service PKI: service %q names no DNS name or IP, so no client could verify it", s.Name)
		}
	}

	ca, err := pkigen.GenerateCA(pkigen.WithOrganization(organization), pkigen.WithValidDuration(valid))
	if err != nil {
		return nil, fmt.Errorf("generate service PKI: %w", err)
	}
	client, err := pkigen.GenerateCert(ca,
		pkigen.WithOrganization(organization),
		pkigen.WithValidDuration(valid),
		pkigen.WithDNSNames("client"),
		pkigen.WithIPs(),
		pkigen.WithExtKeyUsage(x509.ExtKeyUsageClientAuth),
	)
	if err != nil {
		return nil, fmt.Errorf("generate service PKI: client certificate: %w", err)
	}
	p := &PKI{
		CACert:     ca.CertPEM,
		CAKey:      ca.KeyPEM,
		ClientCert: client.CertPEM,
		ClientKey:  client.KeyPEM,
		Services:   make(map[string]ServiceCert, len(services)),
	}
	for _, s := range services {
		leaf, err := pkigen.GenerateCert(ca,
			pkigen.WithOrganization(organization),
			pkigen.WithValidDuration(valid),
			pkigen.WithDNSNames(s.DNSNames...),
			pkigen.WithIPs(s.IPs...),
			pkigen.WithExtKeyUsage(x509.ExtKeyUsageServerAuth),
		)
		if err != nil {
			return nil, fmt.Errorf("generate service PKI: certificate for %q: %w", s.Name, err)
		}
		p.Services[s.Name] = ServiceCert{Cert: leaf.CertPEM, Key: leaf.KeyPEM}
	}
	return p, nil
}

// GenerateRequirements returns what a directory holding Generate output must
// satisfy to be reused: every SAN Generate issues, a serverAuth-only server
// cert, a matching fingerprint, and at least a day of remaining validity so a
// set never expires mid-run.
func GenerateRequirements() VerifyOptions {
	return VerifyOptions{
		DNSNames:           slices.Clone(generateDNSNames),
		IPs:                []net.IP{net.IPv4(127, 0, 0, 1)},
		MinRemaining:       24 * time.Hour,
		RequireFingerprint: true,
		ServerAuthOnly:     true,
	}
}

// pemFile is one file WriteToDir writes.
type pemFile struct {
	name string
	data []byte
	perm os.FileMode
}

// WriteToDir writes the PEM files to the given directory: the CA and client
// pair, then the shared server pair (server.pem, server-key.pem) or, when
// Services is set, <name>.pem and <name>-key.pem per service. It refuses an
// invalid service name before writing anything. Creates the directory
// (including parents) if it does not exist. Cert files get 0644 permissions;
// key files get 0600.
func (p *PKI) WriteToDir(dir string) error {
	for _, name := range slices.Sorted(maps.Keys(p.Services)) {
		if err := validateServiceName(name); err != nil {
			return fmt.Errorf("write PKI: %w", err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create cert dir: %w", err)
	}

	files := []pemFile{
		{"ca.pem", p.CACert, 0o644},
		{"ca-key.pem", p.CAKey, 0o600},
		{"client.pem", p.ClientCert, 0o644},
		{"client-key.pem", p.ClientKey, 0o600},
	}
	if p.ServerCert != nil || len(p.Services) == 0 {
		files = append(files,
			pemFile{"server.pem", p.ServerCert, 0o644},
			pemFile{"server-key.pem", p.ServerKey, 0o600},
		)
	}
	for _, name := range slices.Sorted(maps.Keys(p.Services)) {
		files = append(files,
			pemFile{name + ".pem", p.Services[name].Cert, 0o644},
			pemFile{name + "-key.pem", p.Services[name].Key, 0o600},
		)
	}

	for _, f := range files {
		path := filepath.Join(dir, f.name)
		if err := os.WriteFile(path, f.data, f.perm); err != nil {
			return fmt.Errorf("write %s: %w", f.name, err)
		}
	}
	return nil
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
	// Services replaces the shared server cert: each service must have its own
	// <name>.pem and <name>-key.pem, a serverAuth-only certificate naming
	// exactly that service's DNSNames and IPs, and a key no other file shares.
	// A server.pem or server-key.pem in the directory (the shared layout of
	// earlier releases) fails with ErrSharedServerKey. DNSNames, IPs and
	// ServerAuthOnly describe server.pem and are not used with Services.
	Services []ServiceIdentity
	// ServerAuthOnly requires server.pem to permit server authentication only.
	// A server cert that also permits clientAuth (directly, through
	// anyExtendedKeyUsage, or by listing no extended key usage) lets every
	// service holding server-key.pem connect to the others as a CA-trusted
	// client.
	ServerAuthOnly bool
	// VerifyExtra checks caller-owned files in the directory (e.g. the deploy
	// attestation keypair). Nil skips it.
	VerifyExtra func(dir string) error
}

// VerifyDir validates the baseline every cert directory must meet: the CA,
// server and client PEM files parse, every certificate is valid now, the CA signs the server
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
	if len(opts.Services) > 0 {
		if err := rejectSharedServerFiles(dir); err != nil {
			return err
		}
	}

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

	clientCert, err := verifyLeaf(dir, "client.pem", "client-key.pem", pool, x509.ExtKeyUsageClientAuth, now, opts.MinRemaining)
	if err != nil {
		return err
	}
	if len(opts.Services) > 0 {
		if err := verifyServices(dir, opts, pool, clientCert, now); err != nil {
			return err
		}
	} else if err := verifySharedServer(dir, opts, pool, now); err != nil {
		return err
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

// verifySharedServer checks the single server cert shared by every server:
// chain, key, the required SANs and, if asked, serverAuth-only.
func verifySharedServer(dir string, opts VerifyOptions, pool *x509.CertPool, now time.Time) error {
	serverCert, err := verifyLeaf(dir, "server.pem", "server-key.pem", pool, x509.ExtKeyUsageServerAuth, now, opts.MinRemaining)
	if err != nil {
		return err
	}
	if err := checkCoversSANs("server.pem", serverCert, opts.DNSNames, opts.IPs); err != nil {
		return err
	}
	if opts.ServerAuthOnly {
		return checkServerAuthOnly("server.pem", "server-key.pem", serverCert)
	}
	return nil
}

// rejectSharedServerFiles fails with ErrSharedServerKey when dir holds the
// shared server cert or key of earlier releases.
func rejectSharedServerFiles(dir string) error {
	for _, name := range []string{"server.pem", "server-key.pem"} {
		_, err := os.Lstat(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("stat %s: %w", name, err)
		}
		return fmt.Errorf("%w: %s exists (earlier releases shared server.pem and server-key.pem), so every TLS service shares one key and a compromised service can impersonate all the others. "+
			"Each service needs its own key. Replacing the set mints a new CA, a new client certificate and a new attestation keypair: "+
			"point clients at the new ca.pem and client.pem, and note that attestations signed with the old key do not verify against the new public key",
			ErrSharedServerKey, name)
	}
	return nil
}

// verifyServices checks each service's own cert and key (see
// VerifyOptions.Services) and that no private key is shared between services,
// or with the client identity.
func verifyServices(dir string, opts VerifyOptions, pool *x509.CertPool, clientCert *x509.Certificate, now time.Time) error {
	keyOwner := map[string]string{string(clientCert.RawSubjectPublicKeyInfo): "client.pem"}
	for _, svc := range opts.Services {
		certName, keyName := svc.Name+".pem", svc.Name+"-key.pem"
		cert, err := verifyLeaf(dir, certName, keyName, pool, x509.ExtKeyUsageServerAuth, now, opts.MinRemaining)
		if err != nil {
			return err
		}
		if err := checkServerAuthOnly(certName, keyName, cert); err != nil {
			return err
		}
		if err := checkCoversSANs(certName, cert, svc.DNSNames, svc.IPs); err != nil {
			return err
		}
		if err := checkOnlyOwnSANs(certName, cert, svc); err != nil {
			return err
		}
		if owner, shared := keyOwner[string(cert.RawSubjectPublicKeyInfo)]; shared {
			return fmt.Errorf("%s and %s share a private key, so holding one lets a service impersonate the other", owner, certName)
		}
		keyOwner[string(cert.RawSubjectPublicKeyInfo)] = certName
	}
	return nil
}

// checkCoversSANs fails when cert does not cover every name in dnsNames and ips,
// naming all that are missing.
func checkCoversSANs(name string, cert *x509.Certificate, dnsNames []string, ips []net.IP) error {
	required := slices.Clone(dnsNames)
	for _, ip := range ips {
		required = append(required, ip.String())
	}
	var missing []string
	for _, n := range required {
		if cert.VerifyHostname(n) != nil {
			missing = append(missing, strconv.Quote(n))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s does not cover required SANs: %s", name, strings.Join(missing, ", "))
	}
	return nil
}

// checkOnlyOwnSANs fails when cert names anything beyond svc's own DNS names and
// IPs (another service's hostname, a wildcard, an e-mail address or a URI), so
// the certificate is rejected for every other service's hostname.
func checkOnlyOwnSANs(name string, cert *x509.Certificate, svc ServiceIdentity) error {
	var extra []string
	for _, n := range cert.DNSNames {
		if !slices.Contains(svc.DNSNames, n) {
			extra = append(extra, strconv.Quote(n))
		}
	}
	for _, ip := range cert.IPAddresses {
		if !slices.ContainsFunc(svc.IPs, ip.Equal) {
			extra = append(extra, strconv.Quote(ip.String()))
		}
	}
	for _, e := range cert.EmailAddresses {
		extra = append(extra, strconv.Quote(e))
	}
	for _, u := range cert.URIs {
		extra = append(extra, strconv.Quote(u.String()))
	}
	if len(extra) == 0 {
		return nil
	}
	own := slices.Clone(svc.DNSNames)
	for _, ip := range svc.IPs {
		own = append(own, ip.String())
	}
	return fmt.Errorf("%s names %s, which is not one of its own names (%s); a service certificate may name only that service", name, strings.Join(extra, ", "), strings.Join(own, ", "))
}

// checkServerAuthOnly fails when cert can also be used for client
// authentication, naming the extended key usage that permits it.
func checkServerAuthOnly(certName, keyName string, cert *x509.Certificate) error {
	const msg = "%s permits client authentication (%s), so any service holding %s can connect to the others as a client; it must be serverAuth-only"
	if len(cert.ExtKeyUsage) == 0 && len(cert.UnknownExtKeyUsage) == 0 {
		return fmt.Errorf(msg, certName, "no extended key usage, which permits every use", keyName)
	}
	if !slices.Contains(cert.ExtKeyUsage, x509.ExtKeyUsageServerAuth) {
		return fmt.Errorf(msg, certName, "no serverAuth extended key usage, only unrecognised ones", keyName)
	}
	for _, usage := range cert.ExtKeyUsage {
		switch usage {
		case x509.ExtKeyUsageClientAuth:
			return fmt.Errorf(msg, certName, "extended key usage clientAuth", keyName)
		case x509.ExtKeyUsageAny:
			return fmt.Errorf(msg, certName, "extended key usage any", keyName)
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
// concatenated contents of every *.pem file in dir, in name order. Covering
// every PEM file, not a fixed list, makes a changed, added or removed
// per-service or attestation file change the fingerprint.
func ComputeFingerprint(dir string) (string, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.pem"))
	if err != nil {
		return "", fmt.Errorf("fingerprint: list PEM files in %s: %w", dir, err)
	}
	if len(paths) == 0 {
		return "", fmt.Errorf("fingerprint: no PEM files in %s", dir)
	}
	h := sha256.New()
	for _, path := range paths { // Glob returns them sorted
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("fingerprint: read %s: %w", filepath.Base(path), err)
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
