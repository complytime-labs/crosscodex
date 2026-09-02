// pkg/config/retention_bdd_test.go
package config_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("ParseRetention", func() {
	It("parses year and day suffixes", func() {
		d, indefinite, err := config.ParseRetention("7y")
		Expect(err).NotTo(HaveOccurred())
		Expect(indefinite).To(BeFalse())
		Expect(d).To(Equal(7 * 365 * 24 * time.Hour))
	})
	It("recognizes the indefinite sentinel", func() {
		_, indefinite, err := config.ParseRetention("indefinite")
		Expect(err).NotTo(HaveOccurred())
		Expect(indefinite).To(BeTrue())
	})
	It("accepts Go durations", func() {
		d, _, err := config.ParseRetention("90d")
		Expect(err).NotTo(HaveOccurred())
		Expect(d).To(Equal(90 * 24 * time.Hour))
	})
	It("rejects garbage", func() {
		_, _, err := config.ParseRetention("banana")
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("RetentionConfig.Validate", func() {
	It("accepts an empty archive backend (disabled)", func() {
		rc := config.RetentionConfig{}
		Expect(rc.Validate()).To(Succeed())
	})
	It("accepts local backend", func() {
		rc := config.RetentionConfig{
			Archive: config.ArchiveConfig{Backend: "local"},
		}
		Expect(rc.Validate()).To(Succeed())
	})
	It("accepts s3 backend", func() {
		rc := config.RetentionConfig{
			Archive: config.ArchiveConfig{Backend: "s3"},
		}
		Expect(rc.Validate()).To(Succeed())
	})
	It("rejects unknown archive backends", func() {
		rc := config.RetentionConfig{
			Archive: config.ArchiveConfig{Backend: "azure"},
		}
		Expect(rc.Validate()).To(MatchError(config.ErrInvalidConfig))
	})
	It("rejects unparseable tier values", func() {
		rc := config.RetentionConfig{
			Defaults: config.RetentionTiers{Attestation: "notavalue"},
		}
		Expect(rc.Validate()).To(MatchError(config.ErrInvalidConfig))
	})
	It("rejects negative durations in tiers", func() {
		rc := config.RetentionConfig{
			Defaults: config.RetentionTiers{Attestation: "-1h"},
		}
		Expect(rc.Validate()).To(MatchError(config.ErrInvalidConfig))
	})
	It("accepts indefinite tier values", func() {
		rc := config.RetentionConfig{
			Defaults: config.RetentionTiers{AuditDecisions: "indefinite"},
		}
		Expect(rc.Validate()).To(Succeed())
	})
})

var _ = Describe("RetentionConfig.For", func() {
	It("returns defaults when tenant has no override", func() {
		rc := config.RetentionConfig{
			Defaults: config.RetentionTiers{Attestation: "7y", Catalogs: "90d"},
			Tenants:  map[string]config.RetentionTiers{},
		}
		tiers := rc.For("t1")
		Expect(tiers.Attestation).To(Equal("7y"))
		Expect(tiers.Catalogs).To(Equal("90d"))
	})
	It("overlays non-empty tenant fields onto defaults", func() {
		rc := config.RetentionConfig{
			Defaults: config.RetentionTiers{Attestation: "7y", Catalogs: "90d"},
			Tenants: map[string]config.RetentionTiers{
				"t1": {Attestation: "1y"},
			},
		}
		tiers := rc.For("t1")
		Expect(tiers.Attestation).To(Equal("1y"))
		// Unset override field falls back to default
		Expect(tiers.Catalogs).To(Equal("90d"))
	})
	It("empty override fields fall back to default", func() {
		rc := config.RetentionConfig{
			Defaults: config.RetentionTiers{JobResults: "30d"},
			Tenants: map[string]config.RetentionTiers{
				"t1": {JobResults: ""},
			},
		}
		tiers := rc.For("t1")
		Expect(tiers.JobResults).To(Equal("30d"))
	})
})

var _ = Describe("RetentionConfig defaults round-trip", func() {
	It("propagates retention defaults through the YAML loader", func() {
		tmpHome := GinkgoT().TempDir()
		GinkgoT().Setenv("XDG_CONFIG_HOME", tmpHome)

		loader := config.NewLoader()
		cfg, err := loader.Load(context.Background())
		Expect(err).NotTo(HaveOccurred())

		Expect(cfg.Retention.Defaults.JobResults).To(Equal("90d"))
		Expect(cfg.Retention.Defaults.Catalogs).To(Equal("indefinite"))
	})
})
