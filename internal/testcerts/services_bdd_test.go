package testcerts_test

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testcerts"
)

// svcIdentities are the per-service server identities the specs use. praxis
// stands in for a service a host-side client also reaches, so it names
// localhost and 127.0.0.1 besides its own name.
var svcIdentities = []testcerts.ServiceIdentity{
	{Name: "db", DNSNames: []string{"db"}},
	{Name: "nats", DNSNames: []string{"nats"}},
	{Name: "praxis", DNSNames: []string{"praxis", "localhost"}, IPs: []net.IP{net.IPv4(127, 0, 0, 1)}},
}

// serviceOptions is what a directory holding GenerateServices output must satisfy.
var serviceOptions = testcerts.VerifyOptions{
	Services:           svcIdentities,
	MinRemaining:       24 * time.Hour,
	RequireFingerprint: true,
}

func generateServiceSet(dir string) *testcerts.PKI {
	p, err := testcerts.GenerateServices("CrossCodex Test", 24*time.Hour*30, svcIdentities)
	Expect(err).NotTo(HaveOccurred())
	writeSet(p, dir)
	return p
}

// readKey parses the EC private key PEM dir/name.
func readKey(dir, name string) *ecdsa.PrivateKey {
	data, err := os.ReadFile(filepath.Join(dir, name))
	Expect(err).NotTo(HaveOccurred())
	block, _ := pem.Decode(data)
	Expect(block).NotTo(BeNil())
	key, err := x509.ParseECPrivateKey(block.Bytes)
	Expect(err).NotTo(HaveOccurred())
	return key
}

// reissueServiceCert replaces dir/<name>.pem with a CA-signed serverAuth cert
// for dnsNames that certifies key, then refreshes the fingerprint. Specs use it
// to build sets that break one per-service rule while passing the others.
func reissueServiceCert(dir, name string, dnsNames []string, ekus []x509.ExtKeyUsage, key *ecdsa.PrivateKey, mutate ...func(*x509.Certificate)) {
	caCert := readCertFile(dir, "ca.pem")
	caKey := readKey(dir, "ca-key.pem")
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      readCertFile(dir, name+".pem").Subject,
		NotBefore:    caCert.NotBefore,
		NotAfter:     caCert.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  ekus,
		DNSNames:     dnsNames,
	}
	for _, m := range mutate {
		m(tmpl)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	Expect(err).NotTo(HaveOccurred())
	Expect(os.WriteFile(filepath.Join(dir, name+".pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)).To(Succeed())
	Expect(testcerts.WriteFingerprint(dir)).To(Succeed())
}

var _ = Describe("GenerateServices", func() {
	var p *testcerts.PKI

	BeforeEach(func() {
		var err error
		p, err = testcerts.GenerateServices("CrossCodex Test", time.Hour, svcIdentities)
		Expect(err).NotTo(HaveOccurred())
	})

	It("issues each service a certificate naming only that service, serverAuth-only", func() {
		Expect(p.Services).To(HaveLen(len(svcIdentities)))
		for _, want := range svcIdentities {
			cert := parseCert(p.Services[want.Name].Cert)
			Expect(cert.DNSNames).To(ConsistOf(want.DNSNames), want.Name)
			Expect(cert.IPAddresses).To(HaveLen(len(want.IPs)), want.Name)
			Expect(cert.ExtKeyUsage).To(Equal([]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}), want.Name)
		}
	})

	It("gives every service its own key and no shared server key", func() {
		Expect(p.ServerCert).To(BeEmpty())
		Expect(p.ServerKey).To(BeEmpty())
		seen := map[string]string{}
		for name, sc := range p.Services {
			Expect(seen).NotTo(HaveKey(string(sc.Key)), "%s shares a key with %s", name, seen[string(sc.Key)])
			seen[string(sc.Key)] = name
		}
		Expect(seen).NotTo(HaveKey(string(p.ClientKey)))
	})

	It("keeps the client certificate clientAuth-only", func() {
		Expect(parseCert(p.ClientCert).ExtKeyUsage).To(Equal([]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}))
	})

	DescribeTable("refuses a service list that cannot produce a safe set",
		func(services []testcerts.ServiceIdentity, wantSubstr string) {
			got, err := testcerts.GenerateServices("CrossCodex Test", time.Hour, services)
			Expect(got).To(BeNil())
			Expect(err).To(MatchError(ContainSubstring(wantSubstr)))
		},
		Entry("no services", nil, "at least one service"),
		Entry("a name that would escape the cert dir", []testcerts.ServiceIdentity{{Name: "../x", DNSNames: []string{"x"}}}, `service name "../x"`),
		Entry("a name that would overwrite the CA", []testcerts.ServiceIdentity{{Name: "ca", DNSNames: []string{"ca"}}}, `service name "ca" is reserved`),
		Entry("a name that would overwrite the client identity", []testcerts.ServiceIdentity{{Name: "client", DNSNames: []string{"client"}}}, `service name "client" is reserved`),
		Entry("a name that would overwrite the legacy shared server cert", []testcerts.ServiceIdentity{{Name: "server", DNSNames: []string{"server"}}}, `service name "server" is reserved`),
		Entry("a name that would collide with the attestation keypair", []testcerts.ServiceIdentity{{Name: "attestation", DNSNames: []string{"a"}}}, `service name "attestation" is reserved`),
		Entry("a duplicate name", []testcerts.ServiceIdentity{{Name: "db", DNSNames: []string{"db"}}, {Name: "db", DNSNames: []string{"db2"}}}, `service "db" is listed twice`),
		Entry("a service with no SANs", []testcerts.ServiceIdentity{{Name: "db"}}, `service "db" names no DNS name or IP`),
	)
})

var _ = Describe("WriteToDir with per-service certificates", func() {
	It("writes <name>.pem and <name>-key.pem per service, keys owner-only, and no server.pem", func() {
		dir := filepath.Join(GinkgoT().TempDir(), "certs")
		generateServiceSet(dir)
		Expect(dirEntries(dir)).To(ConsistOf(".fingerprint",
			"ca.pem", "ca-key.pem", "client.pem", "client-key.pem",
			"db.pem", "db-key.pem", "nats.pem", "nats-key.pem", "praxis.pem", "praxis-key.pem"))
		for _, name := range []string{"db-key.pem", "nats-key.pem", "praxis-key.pem"} {
			info, err := os.Stat(filepath.Join(dir, name))
			Expect(err).NotTo(HaveOccurred())
			Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)), name)
		}
	})

	It("refuses a service name that would write outside the directory, leaving nothing behind", func() {
		root := GinkgoT().TempDir()
		dir := filepath.Join(root, "certs")
		p, err := testcerts.GenerateServices("CrossCodex Test", time.Hour, svcIdentities)
		Expect(err).NotTo(HaveOccurred())
		p.Services["../escape"] = p.Services["db"]
		Expect(p.WriteToDir(dir)).To(MatchError(ContainSubstring(`service name "../escape"`)))
		Expect(filepath.Join(root, "escape.pem")).NotTo(BeAnExistingFile())
	})
})

var _ = Describe("VerifyDirWith per-service requirements", func() {
	var dir string

	BeforeEach(func() {
		dir = filepath.Join(GinkgoT().TempDir(), "certs")
		generateServiceSet(dir)
	})

	It("accepts a fresh per-service set", func() {
		Expect(testcerts.VerifyDirWith(dir, serviceOptions)).To(Succeed())
	})

	It("rejects the shared-server-key layout of earlier releases, says why, and leaves the set unchanged", func() {
		legacyDir := filepath.Join(GinkgoT().TempDir(), "legacy")
		legacy, err := testcerts.Generate()
		Expect(err).NotTo(HaveOccurred())
		writeSet(legacy, legacyDir)
		before, err := testcerts.ComputeFingerprint(legacyDir)
		Expect(err).NotTo(HaveOccurred())

		err = testcerts.VerifyDirWith(legacyDir, serviceOptions)

		Expect(err).To(MatchError(testcerts.ErrSharedServerKey))
		Expect(err).To(MatchError(ContainSubstring("server-key.pem")))
		Expect(err).To(MatchError(ContainSubstring("new CA")))
		Expect(err).To(MatchError(ContainSubstring("client certificate")))
		Expect(err).To(MatchError(ContainSubstring("attestation")))
		after, err := testcerts.ComputeFingerprint(legacyDir)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))
		Expect(dirEntries(legacyDir)).To(ContainElement("server.pem"))
	})

	It("rejects a set that carries a shared server cert beside the per-service ones", func() {
		legacy, err := testcerts.Generate()
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(dir, "server.pem"), legacy.ServerCert, 0o644)).To(Succeed())
		Expect(testcerts.WriteFingerprint(dir)).To(Succeed())
		Expect(testcerts.VerifyDirWith(dir, serviceOptions)).To(MatchError(testcerts.ErrSharedServerKey))
	})

	DescribeTable("rejects a per-service set that breaks isolation, naming the file and the fix",
		func(damage func(dir string), wantSubstr string) {
			damage(dir)
			err := testcerts.VerifyDirWith(dir, serviceOptions)
			Expect(err).To(MatchError(ContainSubstring(wantSubstr)))
			Expect(err).To(MatchError(ContainSubstring("regenerate")))
		},
		Entry("a service cert names another service", func(dir string) {
			reissueServiceCert(dir, "db", []string{"db", "nats"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, readKey(dir, "db-key.pem"))
		}, `db.pem names "nats", which is not one of its own names`),
		Entry("a service cert drops a name its clients dial", func(dir string) {
			reissueServiceCert(dir, "praxis", []string{"praxis"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, readKey(dir, "praxis-key.pem"))
		}, `praxis.pem does not cover required SANs: "localhost", "127.0.0.1"`),
		Entry("a service cert also permits client authentication", func(dir string) {
			reissueServiceCert(dir, "nats", []string{"nats"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, readKey(dir, "nats-key.pem"))
		}, "nats.pem permits client authentication"),
		Entry("two services share one private key", func(dir string) {
			shared := readKey(dir, "nats-key.pem")
			reissueServiceCert(dir, "db", []string{"db"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, shared)
			data, err := os.ReadFile(filepath.Join(dir, "nats-key.pem"))
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(dir, "db-key.pem"), data, 0o600)).To(Succeed())
			Expect(testcerts.WriteFingerprint(dir)).To(Succeed())
		}, "db.pem and nats.pem share a private key"),
		Entry("a service cert names an extra IP", func(dir string) {
			reissueServiceCert(dir, "db", []string{"db"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, readKey(dir, "db-key.pem"), func(c *x509.Certificate) { c.IPAddresses = []net.IP{net.ParseIP("10.0.0.9")} })
		}, `db.pem names "10.0.0.9", which is not one of its own names`),
		Entry("a service cert names a wildcard", func(dir string) {
			reissueServiceCert(dir, "db", []string{"db", "*.example"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, readKey(dir, "db-key.pem"))
		}, `db.pem names "*.example", which is not one of its own names`),
		Entry("a service cert names an e-mail address", func(dir string) {
			reissueServiceCert(dir, "db", []string{"db"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, readKey(dir, "db-key.pem"), func(c *x509.Certificate) { c.EmailAddresses = []string{"admin@example.com"} })
		}, `db.pem names "admin@example.com", which is not one of its own names`),
		Entry("a service cert names a URI", func(dir string) {
			reissueServiceCert(dir, "db", []string{"db"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, readKey(dir, "db-key.pem"), func(c *x509.Certificate) { c.URIs = []*url.URL{{Scheme: "spiffe", Host: "example", Path: "/other"}} })
		}, `db.pem names "spiffe://example/other", which is not one of its own names`),
		Entry("a service cert permits every extended key usage", func(dir string) {
			reissueServiceCert(dir, "nats", []string{"nats"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageAny}, readKey(dir, "nats-key.pem"))
		}, "nats.pem permits client authentication (extended key usage any)"),
		Entry("a service cert has no extended key usage", func(dir string) {
			reissueServiceCert(dir, "nats", []string{"nats"}, nil, readKey(dir, "nats-key.pem"))
		}, "nats.pem permits client authentication (no extended key usage"),
		Entry("a service uses the client key", func(dir string) {
			reissueServiceCert(dir, "db", []string{"db"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, readKey(dir, "client-key.pem"))
			data, err := os.ReadFile(filepath.Join(dir, "client-key.pem"))
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(dir, "db-key.pem"), data, 0o600)).To(Succeed())
			Expect(testcerts.WriteFingerprint(dir)).To(Succeed())
		}, "client.pem and db.pem share a private key"),
		Entry("a service key belongs to another certificate", func(dir string) {
			data, err := os.ReadFile(filepath.Join(dir, "nats-key.pem"))
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(dir, "db-key.pem"), data, 0o600)).To(Succeed())
			Expect(testcerts.WriteFingerprint(dir)).To(Succeed())
		}, "db-key.pem does not match db.pem"),
		Entry("a service cert is missing", func(dir string) {
			Expect(os.Remove(filepath.Join(dir, "praxis.pem"))).To(Succeed())
		}, "no such file or directory"),

		Entry("a service file was modified after generation", func(dir string) {
			Expect(os.WriteFile(filepath.Join(dir, "db-key.pem"), []byte("x"), 0o600)).To(Succeed())
		}, "db-key.pem: not valid PEM"),
	)

	It("includes the per-service files in the fingerprint", func() {
		before, err := testcerts.ComputeFingerprint(dir)
		Expect(err).NotTo(HaveOccurred())
		other, err := testcerts.GenerateServices("CrossCodex Test", time.Hour, svcIdentities)
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(dir, "nats.pem"), other.Services["nats"].Cert, 0o644)).To(Succeed())
		after, err := testcerts.ComputeFingerprint(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).NotTo(Equal(before))
	})
})

var _ = Describe("Ensure with per-service requirements", func() {
	generate := func() (*testcerts.PKI, error) {
		return testcerts.GenerateServices("CrossCodex Test", 24*time.Hour*30, svcIdentities)
	}

	It("replaces a shared-server-key set with a per-service one, a new CA and no server key", func() {
		dir := filepath.Join(GinkgoT().TempDir(), "certs")
		legacy, err := testcerts.Generate()
		Expect(err).NotTo(HaveOccurred())
		writeSet(legacy, dir)
		oldCA, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
		Expect(err).NotTo(HaveOccurred())

		generated, err := testcerts.Ensure(dir, testcerts.EnsureOptions{Verify: serviceOptions, Generate: generate})

		Expect(err).NotTo(HaveOccurred())
		Expect(generated).To(BeTrue())
		Expect(testcerts.VerifyDirWith(dir, serviceOptions)).To(Succeed())
		Expect(dirEntries(dir)).NotTo(ContainElements("server.pem", "server-key.pem"))
		newCA, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
		Expect(err).NotTo(HaveOccurred())
		Expect(newCA).NotTo(Equal(oldCA))
	})

	It("keeps a valid per-service set", func() {
		dir := filepath.Join(GinkgoT().TempDir(), "certs")
		opts := testcerts.EnsureOptions{Verify: serviceOptions, Generate: generate}
		_, err := testcerts.Ensure(dir, opts)
		Expect(err).NotTo(HaveOccurred())
		generated, err := testcerts.Ensure(dir, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(generated).To(BeFalse())
	})

	It("covers files written by WriteExtra in the fingerprint", func() {
		dir := filepath.Join(GinkgoT().TempDir(), "certs")
		opts := testcerts.EnsureOptions{
			Verify:     serviceOptions,
			Generate:   generate,
			WriteExtra: testcerts.WriteAttestationKeypair,
		}
		_, err := testcerts.Ensure(dir, opts)
		Expect(err).NotTo(HaveOccurred())
		stored := storedFingerprint(dir)
		Expect(os.WriteFile(filepath.Join(dir, testcerts.AttestationPubFile), []byte("tampered"), 0o644)).To(Succeed())
		computed, err := testcerts.ComputeFingerprint(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(computed).NotTo(Equal(stored))
	})
})

var _ = Describe("Generate", func() {
	It("issues a serverAuth-only server certificate so the test server key cannot act as a client", func() {
		p, err := testcerts.Generate()
		Expect(err).NotTo(HaveOccurred())
		Expect(parseCert(p.ServerCert).ExtKeyUsage).To(Equal([]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}))
		Expect(testcerts.GenerateRequirements().ServerAuthOnly).To(BeTrue())
	})
})
