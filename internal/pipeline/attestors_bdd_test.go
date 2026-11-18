package pipeline_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("NewAttestors", func() {
	var dir string

	// writeKeyPair writes an ECDSA key pair on curve as PEM files named
	// after prefix and returns their paths.
	writeKeyPair := func(curve elliptic.Curve, prefix string) (string, string) {
		key, err := ecdsa.GenerateKey(curve, rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		privDER, err := x509.MarshalECPrivateKey(key)
		Expect(err).NotTo(HaveOccurred())
		pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		Expect(err).NotTo(HaveOccurred())
		priv, pub := filepath.Join(dir, prefix+"-key.pem"), filepath.Join(dir, prefix+"-pub.pem")
		Expect(os.WriteFile(priv, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privDER}), 0o600)).To(Succeed())
		Expect(os.WriteFile(pub, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o600)).To(Succeed())
		return priv, pub
	}
	// signs reports whether att signs a link that verifier accepts, and
	// returns the link's byproducts.
	signs := func(att *pipeline.Attestor, verifier *pipeline.Attestor) (bool, map[string]any) {
		link, err := att.Generator.CreateLink(context.Background(), "step", nil, nil)
		Expect(err).NotTo(HaveOccurred())
		verified, err := verifier.Generator.Verify(context.Background(), link.Raw)
		if err != nil {
			return false, nil
		}
		return true, verified.ByProducts
	}

	BeforeEach(func() { dir = GinkgoT().TempDir() })

	It("builds no attestor and reads no key when attestation is disabled", func() {
		cfg := config.AttestationConfig{
			Enabled: false, PrivateKeyPath: filepath.Join(dir, "absent-key.pem"), PublicKeyPath: filepath.Join(dir, "absent-pub.pem"),
			ExpiryDuration: time.Hour,
		}
		attestors, err := pipeline.NewAttestors(cfg, false, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(attestors.Default).To(BeNil())
		Expect(attestors.Tenants).To(BeEmpty())
	})

	It("builds an attestor from the configured key pair, expiry and storage retries", func() {
		priv, pub := writeKeyPair(elliptic.P256(), "global")
		cfg := config.AttestationConfig{
			Enabled: true, PrivateKeyPath: priv, PublicKeyPath: pub, ExpiryDuration: 48 * time.Hour,
			IncludeByProducts: true, StorageRetries: 4, StorageRetryDelay: 3 * time.Second,
		}
		attestors, err := pipeline.NewAttestors(cfg, false, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(attestors.Default).NotTo(BeNil())
		Expect(attestors.Default.ExpiresIn).To(Equal(48 * time.Hour))
		Expect(attestors.StorageRetries).To(Equal(4))
		Expect(attestors.StorageRetryDelay).To(Equal(3 * time.Second))

		fromFile, err := attestation.NewGenerator(&attestation.FileKeyProvider{PublicKeyPath: pub})
		Expect(err).NotTo(HaveOccurred())
		ok, byProducts := signs(attestors.Default, &pipeline.Attestor{Generator: fromFile})
		Expect(ok).To(BeTrue())
		Expect(byProducts).To(HaveKey("hostname"))
	})

	It("refuses attestation that is enabled without key paths, naming the setting", func() {
		_, err := pipeline.NewAttestors(config.AttestationConfig{Enabled: true, ExpiryDuration: time.Hour}, false, nil, nil)
		Expect(err).To(MatchError(config.ErrMissingRequired))
		Expect(config.FieldErrors(err)).To(ConsistOf(HaveField("Path", "attestation.private_key_path")))
	})

	DescribeTable("refuses a key pair that does not load, naming the settings, the file and both remedies",
		func(override bool, wantSetting string) {
			_, pub := writeKeyPair(elliptic.P256(), "half")
			absent := filepath.Join(dir, "absent-key.pem")
			cfg := config.AttestationConfig{Enabled: true, PrivateKeyPath: absent, PublicKeyPath: pub, ExpiryDuration: time.Hour}
			if override {
				good, goodPub := writeKeyPair(elliptic.P256(), "good")
				cfg.PrivateKeyPath, cfg.PublicKeyPath = good, goodPub
				cfg.TenantOverrides = map[string]config.AttestationOverride{
					"tenant-b": {PrivateKeyPath: &absent, PublicKeyPath: &pub},
				}
			}

			_, err := pipeline.NewAttestors(cfg, false, nil, nil)
			Expect(err).To(MatchError(attestation.ErrKeyNotFound))
			Expect(err.Error()).To(And(
				ContainSubstring(wantSetting+".private_key_path"), ContainSubstring(wantSetting+".public_key_path"),
				ContainSubstring(absent), ContainSubstring("Supply an ECDSA key pair at those paths"),
				ContainSubstring(wantSetting+".enabled: false")))
		},
		Entry("the global settings", false, "attestation"),
		Entry("a tenant override", true, `attestation.tenant_overrides["tenant-b"]`),
	)

	It("refuses a key on a curve FIPS does not approve when tls.fips.enabled is set, and accepts it otherwise", func() {
		priv, pub := writeKeyPair(elliptic.P224(), "p224")
		cfg := config.AttestationConfig{Enabled: true, PrivateKeyPath: priv, PublicKeyPath: pub, ExpiryDuration: time.Hour}

		_, err := pipeline.NewAttestors(cfg, true, nil, nil)
		Expect(err).To(MatchError(attestation.ErrNonFIPSAlgorithm))

		_, err = pipeline.NewAttestors(cfg, false, nil, nil)
		Expect(err).NotTo(HaveOccurred())
	})

	It("resolves each tenant override: disabled, or signing with its own key pair and byproduct setting", func() {
		priv, pub := writeKeyPair(elliptic.P256(), "global")
		tPriv, tPub := writeKeyPair(elliptic.P256(), "tenant")
		yes, no := true, false
		badPath := filepath.Join(dir, "never-read.pem")
		cfg := config.AttestationConfig{
			Enabled: true, PrivateKeyPath: priv, PublicKeyPath: pub, ExpiryDuration: time.Hour, IncludeByProducts: true,
			TenantOverrides: map[string]config.AttestationOverride{
				"off": {Enabled: &no, PrivateKeyPath: &badPath, PublicKeyPath: &badPath},
				"own": {Enabled: &yes, PrivateKeyPath: &tPriv, PublicKeyPath: &tPub, IncludeByProducts: &no},
			},
		}
		attestors, err := pipeline.NewAttestors(cfg, false, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(attestors.Tenants).To(HaveKeyWithValue("off", BeNil()))
		Expect(attestors.Tenants).To(HaveKeyWithValue("own", Not(BeNil())))

		own := attestors.Tenants["own"]
		ok, byProducts := signs(own, own)
		Expect(ok).To(BeTrue())
		Expect(byProducts).NotTo(HaveKey("hostname"))
		ok, _ = signs(own, attestors.Default)
		Expect(ok).To(BeFalse(), "the tenant's links must be signed with its own key")
	})
})

var _ = Describe("Attestors.ForTenant", func() {
	global, own := &pipeline.Attestor{ExpiresIn: time.Hour}, &pipeline.Attestor{ExpiresIn: 2 * time.Hour}

	DescribeTable("resolves a tenant's override before the global attestor",
		func(attestors pipeline.Attestors, tenantID string, want *pipeline.Attestor) {
			Expect(attestors.ForTenant(tenantID)).To(BeIdenticalTo(want))
		},
		Entry("a tenant without an override gets the global attestor",
			pipeline.Attestors{Default: global, Tenants: map[string]*pipeline.Attestor{"tenant-b": own}}, "tenant-a", global),
		Entry("a tenant with an override gets its own",
			pipeline.Attestors{Default: global, Tenants: map[string]*pipeline.Attestor{"tenant-b": own}}, "tenant-b", own),
		Entry("a tenant whose override disables attestation gets none",
			pipeline.Attestors{Default: global, Tenants: map[string]*pipeline.Attestor{"tenant-b": nil}}, "tenant-b", (*pipeline.Attestor)(nil)),
		Entry("no tenant gets one when attestation is disabled globally",
			pipeline.Attestors{}, "tenant-a", (*pipeline.Attestor)(nil)),
	)
})
