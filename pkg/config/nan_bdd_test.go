package config_test

import (
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("NaN in a float setting", func() {
	nan := math.NaN()
	ptr := func(f float64) *float64 { return &f }
	panels := func(c *config.Config) {
		c.Analysis.Requires = config.RequiresConfig{Enabled: true, Models: []string{"m"}, SamplesPerModel: 1,
			ConsensusThreshold: 0.6, MaxSourceChars: 1, MaxTargetChars: 1, MaxTokens: 1}
		// An enabled requires analyzer needs a candidate generator.
		c.Analysis.Candidates.Generators = []config.CandidateGeneratorEntry{{Name: "semantic", Enabled: true}}
		c.Analysis.Artifacts = config.ArtifactsConfig{Enabled: true, Models: []string{"m"}, SamplesPerModel: 1,
			MaxTokens: 1, MaxTextChars: 1, FuzzyThreshold: 0.6}
		c.Analysis.ArtifactAdjudication = config.ArtifactAdjudicationConfig{Enabled: true, Models: []string{"m"},
			SamplesPerModel: 1, MaxTokens: 1, ConsensusThreshold: 0.6, MaxAttempts: 1, LeaseDuration: 1,
			LeaseBatch: 1, PanelConcurrency: 1, RetryBaseDelay: 1, RetryMaxDelay: 1}
	}

	It("leaves the panel fixture valid", func() {
		cfg := validConfig()
		panels(cfg)
		Expect(config.ExportValidateConfig(cfg)).To(Succeed())
	})

	DescribeTable("is rejected, naming the setting",
		func(mutate func(*config.Config), path string) {
			cfg := validConfig()
			panels(cfg)
			mutate(cfg)
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(err).To(MatchError(ContainSubstring(path + " NaN")))
			expectOnlyFieldError(err, path)
		},
		Entry("classification temperature", func(c *config.Config) { c.Analysis.Classification.Temperature = nan }, "analysis.classification.temperature"),
		Entry("relationship sampling temperature", func(c *config.Config) { c.Analysis.Relationship.SamplingTemperature = nan }, "analysis.relationship.sampling_temperature"),
		Entry("candidates min embedding coverage", func(c *config.Config) { c.Analysis.Candidates.MinEmbeddingCoverage = nan }, "analysis.candidates.min_embedding_coverage"),
		Entry("requires consensus threshold", func(c *config.Config) { c.Analysis.Requires.ConsensusThreshold = nan }, "analysis.requires.consensus_threshold"),
		Entry("requires max error rate", func(c *config.Config) { c.Analysis.Requires.MaxErrorRate = nan }, "analysis.requires.max_error_rate"),
		Entry("requires sampling temperature", func(c *config.Config) { c.Analysis.Requires.SamplingTemperature = nan }, "analysis.requires.sampling_temperature"),
		Entry("artifacts sampling temperature", func(c *config.Config) { c.Analysis.Artifacts.SamplingTemperature = nan }, "analysis.artifacts.sampling_temperature"),
		Entry("artifacts fuzzy threshold", func(c *config.Config) { c.Analysis.Artifacts.FuzzyThreshold = nan }, "analysis.artifacts.fuzzy_threshold"),
		Entry("adjudication sampling temperature", func(c *config.Config) { c.Analysis.ArtifactAdjudication.SamplingTemperature = nan }, "analysis.artifact_adjudication.sampling_temperature"),
		Entry("adjudication consensus threshold", func(c *config.Config) { c.Analysis.ArtifactAdjudication.ConsensusThreshold = nan }, "analysis.artifact_adjudication.consensus_threshold"),
		Entry("adjudication max error rate", func(c *config.Config) { c.Analysis.ArtifactAdjudication.MaxErrorRate = nan }, "analysis.artifact_adjudication.max_error_rate"),
		Entry("synthesis type mismatch factor", func(c *config.Config) { c.Synthesis.Viability.TypeMismatchFactor = nan }, "synthesis.viability.type_mismatch_factor"),
		Entry("synthesis skip level factor", func(c *config.Config) { c.Synthesis.Viability.SkipLevelFactor = nan }, "synthesis.viability.skip_level_factor"),
		Entry("synthesis integral to factor", func(c *config.Config) { c.Synthesis.Viability.IntegralToFactor = nan }, "synthesis.viability.integral_to_factor"),
		Entry("synthesis iqr good", func(c *config.Config) { c.Synthesis.Assessment.IQRGood = nan }, "synthesis.assessment.iqr_good"),
		Entry("synthesis iqr poor", func(c *config.Config) { c.Synthesis.Assessment.IQRPoor = nan }, "synthesis.assessment.iqr_poor"),
		Entry("synthesis no rel high", func(c *config.Config) { c.Synthesis.Assessment.NoRelHigh = nan }, "synthesis.assessment.no_rel_high"),
		Entry("synthesis no rel low", func(c *config.Config) { c.Synthesis.Assessment.NoRelLow = nan }, "synthesis.assessment.no_rel_low"),
		Entry("synthesis contested warn", func(c *config.Config) { c.Synthesis.Assessment.ContestedWarn = nan }, "synthesis.assessment.contested_warn"),
		Entry("synthesis actionable warn", func(c *config.Config) { c.Synthesis.Assessment.ActionableWarn = nan }, "synthesis.assessment.actionable_warn"),
		Entry("synthesis confidence threshold", func(c *config.Config) { c.Synthesis.ConfidenceThreshold = nan }, "synthesis.confidence_threshold"),
		Entry("synthesis override confidence threshold", func(c *config.Config) {
			c.Synthesis.TenantOverrides = map[string]config.SynthesisOverride{"acme-corp": {ConfidenceThreshold: ptr(nan)}}
		}, "synthesis.tenant_overrides.acme-corp.confidence_threshold"),
		Entry("synthesis override viability factor", func(c *config.Config) {
			v := c.Synthesis.Viability
			v.SkipLevelFactor = nan
			c.Synthesis.TenantOverrides = map[string]config.SynthesisOverride{"acme-corp": {Viability: &v}}
		}, "synthesis.tenant_overrides.acme-corp.viability.skip_level_factor"),
		Entry("synthesis override assessment threshold", func(c *config.Config) {
			a := c.Synthesis.Assessment
			a.NoRelLow = nan
			c.Synthesis.TenantOverrides = map[string]config.SynthesisOverride{"acme-corp": {Assessment: &a}}
		}, "synthesis.tenant_overrides.acme-corp.assessment.no_rel_low"),
		Entry("synthesis override iqr", func(c *config.Config) {
			a := c.Synthesis.Assessment
			a.IQRPoor = nan
			c.Synthesis.TenantOverrides = map[string]config.SynthesisOverride{"acme-corp": {Assessment: &a}}
		}, "synthesis.tenant_overrides.acme-corp.assessment.iqr_poor"),
	)
})
