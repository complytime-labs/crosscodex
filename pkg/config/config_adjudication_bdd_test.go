package config_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("ArtifactAdjudicationConfig", func() {
	valid := func() config.ArtifactAdjudicationConfig {
		return config.ArtifactAdjudicationConfig{
			Enabled: true, Models: []string{"m1"}, SamplesPerModel: 3, SamplingTemperature: 0.3,
			MaxTokens: 200, ConsensusThreshold: 0.67, MaxErrorRate: 0.34, MaxAttempts: 5,
		}
	}

	It("accepts a complete enabled block", func() {
		c := valid()
		Expect(c.Validate()).To(Succeed())
	})

	It("skips validation when disabled", func() {
		Expect((&config.ArtifactAdjudicationConfig{}).Validate()).To(Succeed())
	})

	It("accepts even samples when allow_even_samples is set", func() {
		c := valid()
		c.SamplesPerModel, c.AllowEvenSamples = 2, true
		Expect(c.Validate()).To(Succeed())
	})

	DescribeTable("rejects an invalid field, naming it",
		func(mutate func(*config.ArtifactAdjudicationConfig), field string) {
			c := valid()
			mutate(&c)
			err := c.Validate()
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring("analysis.artifact_adjudication." + field))
		},
		Entry("no models", func(c *config.ArtifactAdjudicationConfig) { c.Models = nil }, "models"),
		Entry("zero samples", func(c *config.ArtifactAdjudicationConfig) { c.SamplesPerModel = 0 }, "samples_per_model"),
		Entry("even samples", func(c *config.ArtifactAdjudicationConfig) { c.SamplesPerModel = 2 }, "samples_per_model"),
		Entry("negative temperature", func(c *config.ArtifactAdjudicationConfig) { c.SamplingTemperature = -0.1 }, "sampling_temperature"),
		Entry("temperature above 2", func(c *config.ArtifactAdjudicationConfig) { c.SamplingTemperature = 2.1 }, "sampling_temperature"),
		Entry("zero max_tokens", func(c *config.ArtifactAdjudicationConfig) { c.MaxTokens = 0 }, "max_tokens"),
		Entry("threshold below 0.5", func(c *config.ArtifactAdjudicationConfig) { c.ConsensusThreshold = 0.49 }, "consensus_threshold"),
		Entry("threshold above 1", func(c *config.ArtifactAdjudicationConfig) { c.ConsensusThreshold = 1.01 }, "consensus_threshold"),
		Entry("negative error rate", func(c *config.ArtifactAdjudicationConfig) { c.MaxErrorRate = -0.01 }, "max_error_rate"),
		Entry("error rate above 1", func(c *config.ArtifactAdjudicationConfig) { c.MaxErrorRate = 1.01 }, "max_error_rate"),
		Entry("zero max_attempts", func(c *config.ArtifactAdjudicationConfig) { c.MaxAttempts = 0 }, "max_attempts"),
	)

	Describe("loading", func() {
		writeUserConfig := func(body string) {
			dir := GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_CONFIG_HOME", dir)
			Expect(os.MkdirAll(filepath.Join(dir, "crosscodex"), 0o700)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(dir, "crosscodex", "config.yaml"), []byte(body), 0o600)).To(Succeed())
		}

		It("fills every unset field from valid defaults", func() {
			writeUserConfig("analysis:\n  artifact_adjudication:\n    enabled: true\n    models: [m1]\n")
			cfg, err := config.NewLoader().Load(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Analysis.ArtifactAdjudication).To(Equal(config.ArtifactAdjudicationConfig{
				Enabled: true, Models: []string{"m1"}, SamplesPerModel: 3, SamplingTemperature: 0.3,
				MaxTokens: 200, ConsensusThreshold: 0.67, MaxErrorRate: 0.34, MaxAttempts: 5,
			}))
		})

		It("is disabled when not configured", func() {
			GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
			cfg, err := config.NewLoader().Load(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Analysis.ArtifactAdjudication.Enabled).To(BeFalse())
		})

		It("fails to load an enabled block without models", func() {
			writeUserConfig("analysis:\n  artifact_adjudication:\n    enabled: true\n")
			_, err := config.NewLoader().Load(context.Background())
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue(), "got %v", err)
			Expect(err.Error()).To(ContainSubstring("analysis.artifact_adjudication.models"))
		})
	})
})
