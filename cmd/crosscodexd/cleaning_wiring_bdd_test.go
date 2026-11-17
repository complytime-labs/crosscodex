//go:build !integration

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

// The cleaning engine signs every decision, so a missing key pair leaves the
// engine without a signer (it then refuses writes), while a configured pair
// that does not load must stop the daemon rather than degrade to no signer.
var _ = Describe("newCleaningSigner", func() {
	writeKeyPair := func(dir string) (priv, pub string) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		privDER, err := x509.MarshalECPrivateKey(key)
		Expect(err).NotTo(HaveOccurred())
		pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		Expect(err).NotTo(HaveOccurred())
		priv, pub = filepath.Join(dir, "key.pem"), filepath.Join(dir, "pub.pem")
		Expect(os.WriteFile(priv, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privDER}), 0o600)).To(Succeed())
		Expect(os.WriteFile(pub, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o600)).To(Succeed())
		return priv, pub
	}

	DescribeTable("returns no signer, and no error, unless both key paths are set",
		func(priv, pub string) {
			cfg := &config.Config{}
			cfg.Attestation = config.AttestationConfig{PrivateKeyPath: priv, PublicKeyPath: pub}

			signer, err := newCleaningSigner(cfg)
			Expect(err).NotTo(HaveOccurred())
			Expect(signer).To(BeNil())
		},
		Entry("neither path", "", ""),
		Entry("only the private path", "/etc/crosscodex/key.pem", ""),
		Entry("only the public path", "", "/etc/crosscodex/pub.pem"),
	)

	It("returns a signer that signs when both paths hold a key pair", func() {
		priv, pub := writeKeyPair(GinkgoT().TempDir())
		cfg := &config.Config{}
		cfg.Attestation = config.AttestationConfig{PrivateKeyPath: priv, PublicKeyPath: pub}

		signer, err := newCleaningSigner(cfg)
		Expect(err).NotTo(HaveOccurred())
		link, err := signer.CreateLink(GinkgoT().Context(), "cleaning.test", []attestation.Artifact{{URI: "crosscodex:test", Digest: "00"}}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(link.Raw).NotTo(BeEmpty())
	})

	It("refuses when the configured key pair does not load, naming the setting and the file", func() {
		dir := GinkgoT().TempDir()
		missing := filepath.Join(dir, "absent-key.pem")
		cfg := &config.Config{}
		cfg.Attestation = config.AttestationConfig{PrivateKeyPath: missing, PublicKeyPath: filepath.Join(dir, "absent-pub.pem")}

		signer, err := newCleaningSigner(cfg)
		Expect(signer).To(BeNil())
		Expect(err).To(MatchError(attestation.ErrKeyNotFound))
		Expect(err.Error()).To(And(ContainSubstring("attestation.private_key_path"), ContainSubstring(missing)))
	})

	It("refuses a public key that is not the private key's", func() {
		dir := GinkgoT().TempDir()
		priv, _ := writeKeyPair(dir)
		_, otherPub := writeKeyPair(GinkgoT().TempDir())
		cfg := &config.Config{}
		cfg.Attestation = config.AttestationConfig{PrivateKeyPath: priv, PublicKeyPath: otherPub}

		signer, err := newCleaningSigner(cfg)
		Expect(signer).To(BeNil())
		Expect(err).To(MatchError(attestation.ErrKeyMismatch))
	})
})
