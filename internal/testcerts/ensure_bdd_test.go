package testcerts_test

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	It("accepts Generate output under GenerateRequirements, which name every compose service SAN", func() {
		req := testcerts.GenerateRequirements()
		Expect(req.DNSNames).To(ConsistOf("localhost", "db", "nats", "storage", "authn-proxy", "grpc"))
		Expect(req.MinRemaining).To(BeNumerically(">=", 24*time.Hour))
		Expect(req.RequireFingerprint).To(BeTrue())
		Expect(testcerts.VerifyDirWith(dir, req)).To(Succeed())
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
		Entry("the server cert lacks required DNS names and IPs, all named at once", func(string) {},
			testcerts.VerifyOptions{
				DNSNames: []string{"localhost", "praxis", "litellm"},
				IPs:      []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
			}, `server.pem does not cover required SANs: "praxis", "litellm", "::1"`),
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

// ensureHelperDirEnv makes the suite binary run one Ensure against the named
// directory and exit (see TestTestCertsBDD), so specs can race real processes.
const ensureHelperDirEnv = "TESTCERTS_ENSURE_HELPER_DIR"

// strictEnsure regenerates with testcerts.Generate under strictOptions.
var strictEnsure = testcerts.EnsureOptions{Verify: strictOptions, Generate: testcerts.Generate}

// dirEntries lists the names in dir.
func dirEntries(dir string) []string {
	entries, err := os.ReadDir(dir)
	Expect(err).NotTo(HaveOccurred())
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// storedFingerprint reads dir's .fingerprint, failing the spec if absent.
func storedFingerprint(dir string) string {
	fp, err := testcerts.ReadFingerprint(dir)
	Expect(err).NotTo(HaveOccurred())
	Expect(fp).NotTo(BeEmpty())
	return fp
}

var _ = Describe("Ensure", func() {
	var parent, dir string

	BeforeEach(func() {
		parent = GinkgoT().TempDir()
		dir = filepath.Join(parent, "nested", "certs")
	})

	It("generates a verifying set with a fingerprint, keeping the lock file outside the set", func() {
		generated, err := testcerts.Ensure(dir, strictEnsure)
		Expect(err).NotTo(HaveOccurred())
		Expect(generated).To(BeTrue())
		Expect(testcerts.VerifyDirWith(dir, strictOptions)).To(Succeed())
		Expect(dirEntries(dir)).To(ConsistOf(
			"ca.pem", "ca-key.pem", "server.pem", "server-key.pem", "client.pem", "client-key.pem", ".fingerprint"))
		Expect(dirEntries(filepath.Dir(dir))).To(ConsistOf("certs", ".certs.lock"))
	})

	It("leaves a valid set untouched", func() {
		_, err := testcerts.Ensure(dir, strictEnsure)
		Expect(err).NotTo(HaveOccurred())
		before := storedFingerprint(dir)

		generated, err := testcerts.Ensure(dir, strictEnsure)
		Expect(err).NotTo(HaveOccurred())
		Expect(generated).To(BeFalse())
		Expect(storedFingerprint(dir)).To(Equal(before))
	})

	DescribeTable("replaces a stale set with one that verifies",
		func(stale func(dir string), opts testcerts.EnsureOptions) {
			stale(dir)
			Expect(testcerts.VerifyDirWith(dir, opts.Verify)).NotTo(Succeed())
			oldCA, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
			if err != nil {
				oldCA = nil
			}

			generated, err := testcerts.Ensure(dir, opts)
			Expect(err).NotTo(HaveOccurred())
			Expect(generated).To(BeTrue())
			Expect(testcerts.VerifyDirWith(dir, opts.Verify)).To(Succeed())
			Expect(os.ReadFile(filepath.Join(dir, "ca.pem"))).NotTo(Equal(oldCA))
		},
		Entry("a key from another run", func(dir string) {
			p, err := testcerts.Generate()
			Expect(err).NotTo(HaveOccurred())
			other, err := testcerts.Generate()
			Expect(err).NotTo(HaveOccurred())
			p.ServerKey = other.ServerKey
			writeSet(p, dir)
		}, strictEnsure),
		Entry("a server cert missing a newly required SAN", func(dir string) {
			p, err := testcerts.Generate()
			Expect(err).NotTo(HaveOccurred())
			writeSet(p, dir)
		}, testcerts.EnsureOptions{
			Verify: testcerts.VerifyOptions{DNSNames: []string{"praxis"}},
			Generate: func() (*testcerts.PKI, error) {
				return generateWith(pkigen.WithDNSNames("localhost", "praxis")), nil
			},
		}),
		Entry("certificates about to expire", func(dir string) {
			writeSet(generateWith(pkigen.WithValidDuration(time.Hour), pkigen.WithDNSNames("localhost", "db", "nats"),
				pkigen.WithIPs(net.IPv4(127, 0, 0, 1))), dir)
		}, strictEnsure),
		Entry("a partial set left by an interrupted in-place write", func(dir string) {
			p, err := testcerts.Generate()
			Expect(err).NotTo(HaveOccurred())
			Expect(os.MkdirAll(dir, 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(dir, "ca.pem"), p.CACert, 0o644)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(dir, "server.pem"), p.ServerCert[:len(p.ServerCert)/2], 0o644)).To(Succeed())
		}, strictEnsure),
	)

	It("removes staging and set-aside directories left by an interrupted Ensure", func() {
		_, err := testcerts.Ensure(dir, strictEnsure)
		Expect(err).NotTo(HaveOccurred())
		for _, leftover := range []string{".certs.new-123", ".certs.old-456"} {
			Expect(os.MkdirAll(filepath.Join(filepath.Dir(dir), leftover), 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(filepath.Dir(dir), leftover, "ca.pem"), []byte("partial"), 0o644)).To(Succeed())
		}

		_, err = testcerts.Ensure(dir, strictEnsure)
		Expect(err).NotTo(HaveOccurred())
		Expect(dirEntries(filepath.Dir(dir))).To(ConsistOf("certs", ".certs.lock"))
	})

	It("writes caller-owned files into the set and regenerates when they fail verification", func() {
		opts := strictEnsure
		opts.WriteExtra = func(d string) error {
			return os.WriteFile(filepath.Join(d, "extra.pem"), []byte("extra"), 0o600)
		}
		opts.Verify.VerifyExtra = func(d string) error {
			_, err := os.Stat(filepath.Join(d, "extra.pem"))
			return err
		}
		_, err := testcerts.Ensure(dir, opts)
		Expect(err).NotTo(HaveOccurred())
		info, err := os.Stat(filepath.Join(dir, "extra.pem"))
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)))
		before := storedFingerprint(dir)

		Expect(os.Remove(filepath.Join(dir, "extra.pem"))).To(Succeed())
		generated, err := testcerts.Ensure(dir, opts)
		Expect(err).NotTo(HaveOccurred())
		Expect(generated).To(BeTrue())
		Expect(filepath.Join(dir, "extra.pem")).To(BeAnExistingFile())
		Expect(storedFingerprint(dir)).NotTo(Equal(before))
	})

	It("refuses to install a generated set that fails the requirements and keeps the existing set", func() {
		p, err := testcerts.Generate()
		Expect(err).NotTo(HaveOccurred())
		writeSet(p, dir)
		before := storedFingerprint(dir)

		opts := strictEnsure
		opts.Verify.DNSNames = []string{"praxis"}
		_, err = testcerts.Ensure(dir, opts)
		Expect(err).To(MatchError(And(
			ContainSubstring(`does not cover required SANs: "praxis"`),
			ContainSubstring("fix the generator options"))))
		Expect(storedFingerprint(dir)).To(Equal(before))
		Expect(dirEntries(filepath.Dir(dir))).To(ConsistOf("certs", ".certs.lock"))
	})

	It("installs the directory with DirMode when set", func() {
		opts := strictEnsure
		opts.DirMode = 0o700
		_, err := testcerts.Ensure(dir, opts)
		Expect(err).NotTo(HaveOccurred())
		info, err := os.Stat(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o700)))
	})

	Describe("with KeepUnmanaged", func() {
		var opts testcerts.EnsureOptions

		BeforeEach(func() {
			opts = strictEnsure
			opts.KeepUnmanaged = true
		})

		// snapshot returns every file in dir by name, so specs can assert a set was not touched.
		snapshot := func(dir string) map[string][]byte {
			files := map[string][]byte{}
			for _, name := range dirEntries(dir) {
				data, err := os.ReadFile(filepath.Join(dir, name))
				Expect(err).NotTo(HaveOccurred())
				files[name] = data
			}
			return files
		}

		It("keeps a valid set it did not generate, without adding a fingerprint", func() {
			p, err := testcerts.Generate()
			Expect(err).NotTo(HaveOccurred())
			Expect(p.WriteToDir(dir)).To(Succeed())
			before := snapshot(dir)

			generated, err := testcerts.Ensure(dir, opts)
			Expect(err).NotTo(HaveOccurred())
			Expect(generated).To(BeFalse())
			Expect(snapshot(dir)).To(Equal(before))
		})

		It("keeps a set it did not generate that is valid now but inside the renewal margin, since it cannot renew it", func() {
			writeOnly := generateWith(pkigen.WithValidDuration(time.Hour), pkigen.WithDNSNames("localhost", "db", "nats"),
				pkigen.WithIPs(net.IPv4(127, 0, 0, 1)))
			Expect(writeOnly.WriteToDir(dir)).To(Succeed())
			before := snapshot(dir)

			generated, err := testcerts.Ensure(dir, opts)
			Expect(err).NotTo(HaveOccurred())
			Expect(generated).To(BeFalse())
			Expect(snapshot(dir)).To(Equal(before))
		})

		It("refuses to replace an invalid set it did not generate and leaves it unchanged", func() {
			p, err := testcerts.Generate()
			Expect(err).NotTo(HaveOccurred())
			other, err := testcerts.Generate()
			Expect(err).NotTo(HaveOccurred())
			p.ServerKey = other.ServerKey
			Expect(p.WriteToDir(dir)).To(Succeed())
			before := snapshot(dir)

			_, err = testcerts.Ensure(dir, opts)
			Expect(err).To(MatchError(testcerts.ErrUnmanagedDir))
			Expect(err).To(MatchError(And(
				ContainSubstring("server-key.pem does not match server.pem"),
				ContainSubstring("remove the directory"))))
			Expect(snapshot(dir)).To(Equal(before))
		})

		It("still regenerates a set it generated that has gone stale", func() {
			_, err := testcerts.Ensure(dir, opts)
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(dir, "ca.pem"), []byte("corrupted"), 0o644)).To(Succeed())

			generated, err := testcerts.Ensure(dir, opts)
			Expect(err).NotTo(HaveOccurred())
			Expect(generated).To(BeTrue())
			Expect(testcerts.VerifyDirWith(dir, strictOptions)).To(Succeed())
		})

		It("generates into an existing empty directory", func() {
			Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
			generated, err := testcerts.Ensure(dir, opts)
			Expect(err).NotTo(HaveOccurred())
			Expect(generated).To(BeTrue())
		})
	})

	It("generates exactly once when many goroutines ensure the same directory", func() {
		const n = 8
		results := make(chan bool, n)
		errs := make(chan error, n)
		for range n {
			go func() {
				defer GinkgoRecover()
				generated, err := testcerts.Ensure(dir, strictEnsure)
				errs <- err
				results <- generated
			}()
		}
		generatedCount := 0
		for range n {
			Expect(<-errs).NotTo(HaveOccurred())
			if <-results {
				generatedCount++
			}
		}
		Expect(generatedCount).To(Equal(1))
		Expect(testcerts.VerifyDirWith(dir, strictOptions)).To(Succeed())
	})

	It("generates exactly once when many processes ensure the same directory", func() {
		const n = 4
		cmds := make([]*exec.Cmd, n)
		outs := make([]*strings.Builder, n)
		for i := range n {
			outs[i] = &strings.Builder{}
			cmds[i] = exec.Command(os.Args[0], "-test.run=^TestTestCertsBDD$")
			cmds[i].Env = append(os.Environ(), ensureHelperDirEnv+"="+dir)
			cmds[i].Stdout = outs[i]
			cmds[i].Stderr = GinkgoWriter
			Expect(cmds[i].Start()).To(Succeed())
		}
		generatedCount := 0
		for i := range n {
			Expect(cmds[i].Wait()).To(Succeed())
			if strings.Contains(outs[i].String(), "generated=true") {
				generatedCount++
			}
		}
		Expect(generatedCount).To(Equal(1))
		Expect(testcerts.VerifyDirWith(dir, strictOptions)).To(Succeed())
	})
})
