package testcerts_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testcerts"
	pkigen "github.com/complytime-labs/crosscodex/pkg/tlsconfig/pki"
)

// generateWith builds a PKI with the given generator options so specs can
// produce sets that differ from testcerts.Generate (short validity, other SANs).
func generateWith(opts ...pkigen.Option) *testcerts.PKI {
	bundle, err := pkigen.GenerateDevPKI(opts...)
	Expect(err).NotTo(HaveOccurred())
	return &testcerts.PKI{
		CACert:     bundle.CA.CertPEM,
		CAKey:      bundle.CA.KeyPEM,
		ServerCert: bundle.Server.CertPEM,
		ServerKey:  bundle.Server.KeyPEM,
		ClientCert: bundle.Client.CertPEM,
		ClientKey:  bundle.Client.KeyPEM,
	}
}

// writeSet writes p plus its fingerprint into dir.
func writeSet(p *testcerts.PKI, dir string) {
	Expect(p.WriteToDir(dir)).To(Succeed())
	Expect(testcerts.WriteFingerprint(dir)).To(Succeed())
}

// strictOptions is what the integration generator requires of testcerts.Generate output.
var strictOptions = testcerts.VerifyOptions{
	DNSNames:           []string{"localhost", "db", "nats"},
	IPs:                []net.IP{net.IPv4(127, 0, 0, 1)},
	MinRemaining:       24 * time.Hour,
	RequireFingerprint: true,
}

var _ = Describe("VerifyDirWith", func() {
	var dir string

	BeforeEach(func() {
		dir = filepath.Join(GinkgoT().TempDir(), "certs")
		p, err := testcerts.Generate()
		Expect(err).NotTo(HaveOccurred())
		writeSet(p, dir)
	})

	It("accepts a fresh set that meets every requirement", func() {
		Expect(testcerts.VerifyDirWith(dir, strictOptions)).To(Succeed())
	})

	DescribeTable("rejects a stale or inconsistent set with an error naming the problem and the fix",
		func(damage func(dir string), opts testcerts.VerifyOptions, wantSubstr string) {
			damage(dir)
			err := testcerts.VerifyDirWith(dir, opts)
			Expect(err).To(MatchError(ContainSubstring(wantSubstr)))
			Expect(err).To(MatchError(ContainSubstring("regenerate")))
		},
		Entry("the server key belongs to another certificate", func(dir string) {
			other, err := testcerts.Generate()
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(dir, "server-key.pem"), other.ServerKey, 0o600)).To(Succeed())
		}, testcerts.VerifyOptions{}, "server-key.pem does not match server.pem"),
		Entry("the client key belongs to another certificate", func(dir string) {
			other, err := testcerts.Generate()
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(dir, "client-key.pem"), other.ClientKey, 0o600)).To(Succeed())
		}, testcerts.VerifyOptions{}, "client-key.pem does not match client.pem"),
		Entry("the CA key belongs to another CA", func(dir string) {
			other, err := testcerts.Generate()
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(dir, "ca-key.pem"), other.CAKey, 0o600)).To(Succeed())
		}, testcerts.VerifyOptions{}, "ca-key.pem does not match ca.pem"),
		Entry("the server cert lacks a required DNS name", func(string) {},
			testcerts.VerifyOptions{DNSNames: []string{"localhost", "praxis"}}, `server.pem does not cover required name "praxis"`),
		Entry("the server cert lacks a required IP", func(string) {},
			testcerts.VerifyOptions{IPs: []net.IP{net.IPv6loopback}}, `server.pem does not cover required IP "::1"`),
		Entry("a certificate expires within the required margin", func(dir string) {
			writeSet(generateWith(pkigen.WithValidDuration(time.Hour)), dir)
		}, testcerts.VerifyOptions{MinRemaining: 24 * time.Hour}, "expires within 24h0m0s"),
		Entry("the fingerprint is missing", func(dir string) {
			Expect(os.Remove(filepath.Join(dir, ".fingerprint"))).To(Succeed())
		}, testcerts.VerifyOptions{RequireFingerprint: true}, ".fingerprint"),
		Entry("the fingerprint does not match the PEM files", func(dir string) {
			Expect(os.WriteFile(filepath.Join(dir, ".fingerprint"), []byte("deadbeef\n"), 0o644)).To(Succeed())
		}, testcerts.VerifyOptions{RequireFingerprint: true}, ".fingerprint does not match"),
		Entry("a caller-owned file check fails", func(string) {},
			testcerts.VerifyOptions{VerifyExtra: func(string) error { return errors.New("attestation-key.pem: missing") }},
			"attestation-key.pem: missing"),
	)
})
