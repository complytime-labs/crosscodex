package testcerts_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testcerts"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
)

var _ = Describe("Attestation key pair", func() {
	var dir string

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
	})

	It("writes a pair attestation.LoadFileKeys accepts, with an owner-only private key", func() {
		Expect(testcerts.WriteAttestationKeypair(dir)).To(Succeed())

		_, err := attestation.LoadFileKeys(
			filepath.Join(dir, testcerts.AttestationKeyFile),
			filepath.Join(dir, testcerts.AttestationPubFile), false)
		Expect(err).NotTo(HaveOccurred())

		info, err := os.Stat(filepath.Join(dir, testcerts.AttestationKeyFile))
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)))
		Expect(testcerts.VerifyAttestationKeypair(dir)).To(Succeed())
	})

	It("fails to write into a directory that does not exist", func() {
		err := testcerts.WriteAttestationKeypair(filepath.Join(dir, "missing"))

		Expect(err).To(MatchError(ContainSubstring("write attestation private key")))
	})

	DescribeTable("verification rejects a broken set with an error naming the file",
		func(break_ func(dir string), want string) {
			Expect(testcerts.WriteAttestationKeypair(dir)).To(Succeed())
			break_(dir)

			Expect(testcerts.VerifyAttestationKeypair(dir)).To(MatchError(ContainSubstring(want)))
		},
		Entry("a missing public key", func(d string) {
			Expect(os.Remove(filepath.Join(d, testcerts.AttestationPubFile))).To(Succeed())
		}, testcerts.AttestationPubFile),
		Entry("a missing private key", func(d string) {
			Expect(os.Remove(filepath.Join(d, testcerts.AttestationKeyFile))).To(Succeed())
		}, testcerts.AttestationKeyFile),
		Entry("a private key that is not PEM", func(d string) {
			Expect(os.WriteFile(filepath.Join(d, testcerts.AttestationKeyFile), []byte("junk"), 0o600)).To(Succeed())
		}, "not valid PEM"),
		Entry("a public key from another pair", func(d string) {
			other := GinkgoT().TempDir()
			Expect(testcerts.WriteAttestationKeypair(other)).To(Succeed())
			pub, err := os.ReadFile(filepath.Join(other, testcerts.AttestationPubFile))
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(d, testcerts.AttestationPubFile), pub, 0o644)).To(Succeed())
		}, "does not match"),
	)
})
