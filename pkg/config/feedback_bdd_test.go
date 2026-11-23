package config_test

import (
	"context"
	"math"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("FeedbackConfig", func() {
	// load reads the compiled defaults under a user config file holding yaml.
	load := func(yaml string) (*config.Config, error) {
		home := GinkgoT().TempDir()
		GinkgoT().Setenv("XDG_CONFIG_HOME", home)
		if yaml != "" {
			Expect(os.MkdirAll(filepath.Join(home, "crosscodex"), 0o755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(home, "crosscodex", "config.yaml"), []byte(yaml), 0o600)).To(Succeed())
		}
		return config.NewLoader().Load(context.Background())
	}

	It("defaults both thresholds to 0.5", func() {
		cfg, err := load("")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Feedback).To(Equal(config.FeedbackConfig{MinAgreement: 0.5, LowConfidence: 0.5}))
	})

	It("takes both thresholds from a config file, including the bounds 0 and 1", func() {
		cfg, err := load("feedback:\n  min_agreement: 1\n  low_confidence: 0\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Feedback).To(Equal(config.FeedbackConfig{MinAgreement: 1, LowConfidence: 0}))
	})

	It("is global only: a tenant_overrides key is refused as unknown", func() {
		_, err := load("feedback:\n  tenant_overrides:\n    acme-corp:\n      min_agreement: 0.8\n")
		Expect(err).To(MatchError(config.ErrUnknownKey))
		Expect(err).To(MatchError(ContainSubstring("feedback.tenant_overrides")))
	})

	DescribeTable("rejects a threshold outside [0, 1], naming the setting and what to set",
		func(mutate func(*config.FeedbackConfig), path, got string) {
			cfg := validConfig()
			mutate(&cfg.Feedback)
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(err).To(MatchError(ContainSubstring(path + " must be between 0 and 1, got " + got)))
			Expect(err).To(MatchError(ContainSubstring("set it to")))
			expectOnlyFieldError(err, path)
		},
		Entry("min_agreement below 0", func(f *config.FeedbackConfig) { f.MinAgreement = -0.1 }, "feedback.min_agreement", "-0.1"),
		Entry("min_agreement above 1", func(f *config.FeedbackConfig) { f.MinAgreement = 1.5 }, "feedback.min_agreement", "1.5"),
		Entry("min_agreement NaN", func(f *config.FeedbackConfig) { f.MinAgreement = math.NaN() }, "feedback.min_agreement", "NaN"),
		Entry("low_confidence below 0", func(f *config.FeedbackConfig) { f.LowConfidence = -1 }, "feedback.low_confidence", "-1"),
		Entry("low_confidence above 1", func(f *config.FeedbackConfig) { f.LowConfidence = 2 }, "feedback.low_confidence", "2"),
		Entry("low_confidence NaN", func(f *config.FeedbackConfig) { f.LowConfidence = math.NaN() }, "feedback.low_confidence", "NaN"),
	)

	It("rejects an invalid threshold from a config file, naming the file", func() {
		_, err := load("feedback:\n  min_agreement: 1.2\n")
		Expect(err).To(MatchError(config.ErrInvalidConfig))
		Expect(err).To(MatchError(ContainSubstring("feedback.min_agreement must be between 0 and 1, got 1.2")))
		Expect(err).To(MatchError(ContainSubstring("config.yaml")))
	})
})
