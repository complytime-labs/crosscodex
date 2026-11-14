package config_test

import (
	"context"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("CritiqueConfig", func() {
	validCritique := func() config.CritiqueConfig {
		return config.CritiqueConfig{
			Enabled:             true,
			Models:              []string{"panel-1", "panel-2", "panel-3"},
			SamplesPerModel:     1,
			SamplingTemperature: 0.3,
			MaxTokens:           300,
			ConsensusThreshold:  0.66,
			MaxErrorRate:        0.34,
			MaxSourceChars:      1500,
			MaxTargetChars:      800,
		}
	}

	It("accepts a complete enabled block", func() {
		c := validCritique()
		Expect(c.Validate()).To(Succeed())
	})

	It("skips validation when disabled", func() {
		c := config.CritiqueConfig{ConsensusThreshold: 2.0, Models: []string{"a", "a"}}
		Expect(c.Validate()).To(Succeed())
	})

	It("accepts an even sample count when allow_even_samples is set", func() {
		c := validCritique()
		c.SamplesPerModel, c.AllowEvenSamples = 2, true
		Expect(c.Validate()).To(Succeed())
	})

	DescribeTable("rejects an invalid field, naming it and wrapping ErrInvalidConfig",
		func(mutate func(*config.CritiqueConfig), field, msg string) {
			c := validCritique()
			mutate(&c)
			err := c.Validate()
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(err.Error()).To(ContainSubstring(field))
			Expect(err.Error()).To(ContainSubstring(msg))
			expectOnlyFieldError(err, field)
		},
		Entry("consensus_threshold below 0.5", func(c *config.CritiqueConfig) { c.ConsensusThreshold = 0.4 }, "analysis.critique.consensus_threshold", "must be in range [0.5, 1.0]"),
		Entry("consensus_threshold above 1.0", func(c *config.CritiqueConfig) { c.ConsensusThreshold = 1.1 }, "analysis.critique.consensus_threshold", "must be in range [0.5, 1.0]"),
		Entry("samples_per_model of 0", func(c *config.CritiqueConfig) { c.SamplesPerModel = 0 }, "analysis.critique.samples_per_model", "must be positive"),
		Entry("an even samples_per_model without allow_even_samples", func(c *config.CritiqueConfig) { c.SamplesPerModel = 2 }, "analysis.critique.samples_per_model", "must be odd unless allow_even_samples=true"),
		Entry("empty models while enabled", func(c *config.CritiqueConfig) { c.Models = nil }, "analysis.critique.models", "must not be empty when enabled"),
		Entry("max_error_rate above 1.0", func(c *config.CritiqueConfig) { c.MaxErrorRate = 1.1 }, "analysis.critique.max_error_rate", "must be in range [0.0, 1.0]"),
		Entry("sampling_temperature above 2.0", func(c *config.CritiqueConfig) { c.SamplingTemperature = 2.1 }, "analysis.critique.sampling_temperature", "must be in range [0.0, 2.0]"),
		Entry("max_tokens of 0", func(c *config.CritiqueConfig) { c.MaxTokens = 0 }, "analysis.critique.max_tokens", "must be positive"),
		Entry("max_source_chars of 0", func(c *config.CritiqueConfig) { c.MaxSourceChars = 0 }, "analysis.critique.max_source_chars", "must be positive"),
		Entry("max_target_chars of 0", func(c *config.CritiqueConfig) { c.MaxTargetChars = 0 }, "analysis.critique.max_target_chars", "must be positive"),
	)
})

// enablePairPrerequisites enables the candidate generator an enabled
// relationship or requires analyzer needs, so a spec reaches the panel check.
func enablePairPrerequisites(c *config.Config) {
	c.Analysis.Candidates.Generators = []config.CandidateGeneratorEntry{{Name: "semantic", Enabled: true}}
}

var _ = Describe("panel models duplicate-alias rule", func() {
	dup := []string{"panel-1", "panel-2", "panel-2"}

	DescribeTable("rejects a repeated alias, naming both entries",
		func(mutate func(*config.Config)) {
			cfg := validConfig()
			mutate(cfg)
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(err.Error()).To(ContainSubstring(`lists "panel-2" twice (entries 1 and 2)`))
			Expect(err.Error()).To(ContainSubstring("Remove the duplicate"))
		},
		Entry("relationship", func(c *config.Config) {
			c.Analysis.Relationship.Enabled, c.Analysis.Relationship.Models = true, dup
			enablePairPrerequisites(c)
		}),
		Entry("requires", func(c *config.Config) {
			c.Analysis.Requires = config.RequiresConfig{
				Enabled: true, Models: dup, SamplesPerModel: 1, ConsensusThreshold: 0.66,
				MaxErrorRate: 0.34, MaxSourceChars: 1500, MaxTargetChars: 800, MaxTokens: 300,
			}
			enablePairPrerequisites(c)
		}),
		Entry("critique", func(c *config.Config) {
			c.Analysis.Critique = config.CritiqueConfig{
				Enabled: true, Models: dup, SamplesPerModel: 1, ConsensusThreshold: 0.66,
				MaxErrorRate: 0.34, MaxSourceChars: 1500, MaxTargetChars: 800, MaxTokens: 300,
			}
		}),
		Entry("artifact adjudication", func(c *config.Config) {
			c.Analysis.ArtifactAdjudication = config.ArtifactAdjudicationConfig{
				Enabled: true, Models: dup, SamplesPerModel: 3, MaxTokens: 200, ConsensusThreshold: 0.66,
				MaxErrorRate: 0.34, MaxAttempts: 5, LeaseDuration: 15 * time.Minute, LeaseBatch: 10,
				PanelConcurrency: 4, RetryBaseDelay: time.Minute, RetryMaxDelay: 24 * time.Hour,
			}
		}),
	)

	It("accepts distinct aliases", func() {
		cfg := validConfig()
		cfg.Analysis.Relationship.Enabled = true
		cfg.Analysis.Relationship.Models = []string{"panel-1", "panel-2", "panel-3"}
		enablePairPrerequisites(cfg)
		Expect(config.ExportValidateConfig(cfg)).To(Succeed())
	})
})

var _ = Describe("panel and critique defaults", func() {
	loadDefaults := func() *config.Config {
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		cfg, err := config.NewLoader().Load(context.Background())
		Expect(err).NotTo(HaveOccurred())
		return cfg
	}

	It("shares no panel model by default", func() {
		cfg := loadDefaults()
		Expect(cfg.Analysis.Relationship.AllowSharedPanelModels).To(BeFalse())
		Expect(cfg.Analysis.Requires.AllowSharedPanelModels).To(BeFalse())
		Expect(cfg.Analysis.Critique.AllowSharedPanelModels).To(BeFalse())
	})

	It("enables critique over the three panel aliases", func() {
		c := loadDefaults().Analysis.Critique
		Expect(c.Enabled).To(BeTrue())
		Expect(c.Models).To(Equal([]string{"panel-1", "panel-2", "panel-3"}))
		Expect(c.SamplesPerModel).To(Equal(1))
		Expect(c.AllowEvenSamples).To(BeFalse())
		Expect(c.ConsensusThreshold).To(Equal(0.66))
	})

	DescribeTable("loads a valid override of each critique field from the user config",
		func(yamlLine string, got func(config.CritiqueConfig) any, want any) {
			dir := GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_CONFIG_HOME", dir)
			Expect(os.MkdirAll(filepath.Join(dir, "crosscodex"), 0o700)).To(Succeed())
			body := "analysis:\n  critique:\n    " + yamlLine + "\n"
			Expect(os.WriteFile(filepath.Join(dir, "crosscodex", "config.yaml"), []byte(body), 0o600)).To(Succeed())

			cfg, err := config.NewLoader().Load(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(got(cfg.Analysis.Critique)).To(Equal(want))
		},
		Entry("enabled", "enabled: false", func(c config.CritiqueConfig) any { return c.Enabled }, false),
		Entry("models", "models: [a, b]", func(c config.CritiqueConfig) any { return c.Models }, []string{"a", "b"}),
		Entry("samples_per_model", "samples_per_model: 3", func(c config.CritiqueConfig) any { return c.SamplesPerModel }, 3),
		Entry("allow_even_samples", "{allow_even_samples: true, samples_per_model: 2}", func(c config.CritiqueConfig) any { return c.AllowEvenSamples }, true),
		Entry("sampling_temperature", "sampling_temperature: 1.5", func(c config.CritiqueConfig) any { return c.SamplingTemperature }, 1.5),
		Entry("max_tokens", "max_tokens: 500", func(c config.CritiqueConfig) any { return c.MaxTokens }, 500),
		Entry("consensus_threshold", "consensus_threshold: 0.8", func(c config.CritiqueConfig) any { return c.ConsensusThreshold }, 0.8),
		Entry("max_error_rate", "max_error_rate: 0.1", func(c config.CritiqueConfig) any { return c.MaxErrorRate }, 0.1),
		Entry("max_source_chars", "max_source_chars: 900", func(c config.CritiqueConfig) any { return c.MaxSourceChars }, 900),
		Entry("max_target_chars", "max_target_chars: 400", func(c config.CritiqueConfig) any { return c.MaxTargetChars }, 400),
		Entry("allow_shared_panel_models", "allow_shared_panel_models: true", func(c config.CritiqueConfig) any { return c.AllowSharedPanelModels }, true),
	)

	DescribeTable("binds the allow_shared_panel_models env var",
		func(env string, field func(*config.Config) bool) {
			GinkgoT().Setenv(env, "true")
			Expect(field(loadDefaults())).To(BeTrue())
		},
		Entry("relationship", "CROSSCODEX_ANALYSIS_RELATIONSHIP_ALLOW_SHARED_PANEL_MODELS", func(c *config.Config) bool { return c.Analysis.Relationship.AllowSharedPanelModels }),
		Entry("requires", "CROSSCODEX_ANALYSIS_REQUIRES_ALLOW_SHARED_PANEL_MODELS", func(c *config.Config) bool { return c.Analysis.Requires.AllowSharedPanelModels }),
		Entry("critique", "CROSSCODEX_ANALYSIS_CRITIQUE_ALLOW_SHARED_PANEL_MODELS", func(c *config.Config) bool { return c.Analysis.Critique.AllowSharedPanelModels }),
	)
})
