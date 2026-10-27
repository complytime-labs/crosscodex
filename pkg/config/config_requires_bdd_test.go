package config_test

import (
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("Candidate Configuration", func() {
	It("has empty generators by default", func() {
		cfg := config.CandidateConfig{}
		Expect(cfg.Generators).To(BeEmpty())
	})

	Describe("CandidateGeneratorEntry", func() {
		It("stores all fields correctly", func() {
			entry := config.CandidateGeneratorEntry{
				Name:    "semantic",
				Enabled: true,
				Weight:  1.5,
				Config: map[string]interface{}{
					"top_k": 10,
				},
			}

			Expect(entry.Name).To(Equal("semantic"))
			Expect(entry.Enabled).To(BeTrue())
			Expect(entry.Weight).To(Equal(1.5))
			Expect(entry.Config["top_k"]).To(Equal(10))
		})
	})

	Describe("validation", func() {
		DescribeTable("min_embedding_coverage validation",
			func(coverage float64, shouldErr bool) {
				cfg := config.CandidateConfig{MinEmbeddingCoverage: coverage, EmbedModel: "m"}

				err := cfg.Validate()
				if shouldErr {
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("min_embedding_coverage"))
				} else {
					Expect(err).NotTo(HaveOccurred())
				}
			},
			Entry("below range", -0.1, true),
			Entry("min valid", 0.0, false),
			Entry("mid valid", 0.8, false),
			Entry("max valid", 1.0, false),
			Entry("above range", 1.5, true),
		)

		It("requires a non-empty embed_model", func() {
			cfg := config.CandidateConfig{MinEmbeddingCoverage: 0.5, EmbedModel: ""}
			err := cfg.Validate()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("embed_model"))
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
		})
	})
})

var _ = Describe("Requires Configuration", func() {
	Describe("structure", func() {
		It("stores all fields correctly", func() {
			cfg := config.RequiresConfig{
				Enabled:             true,
				Models:              []string{"model-a", "model-b"},
				SamplesPerModel:     3,
				AllowEvenSamples:    false,
				SamplingTemperature: 0.7,
				MaxTokens:           500,
				ConsensusThreshold:  0.6,
				MaxErrorRate:        0.2,
				MaxSourceChars:      2000,
				MaxTargetChars:      2000,
			}

			Expect(cfg.Enabled).To(BeTrue())
			Expect(cfg.SamplesPerModel).To(Equal(3))
			Expect(cfg.ConsensusThreshold).To(Equal(0.6))
		})
	})

	Describe("validation", func() {
		// validRequires returns a RequiresConfig that passes validation,
		// so individual tests can mutate only the field under test.
		validRequires := func() config.RequiresConfig {
			return config.RequiresConfig{
				Enabled:             true,
				Models:              []string{"model-a"},
				SamplesPerModel:     3,
				AllowEvenSamples:    false,
				ConsensusThreshold:  0.6,
				MaxErrorRate:        0.2,
				MaxSourceChars:      1000,
				MaxTargetChars:      1000,
				SamplingTemperature: 0.7,
				MaxTokens:           500,
			}
		}

		Context("sample count parity", func() {
			It("rejects even samples when AllowEvenSamples is false", func() {
				cfg := validRequires()
				cfg.SamplesPerModel = 4
				cfg.AllowEvenSamples = false

				err := cfg.Validate()
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("must be odd"))
			})

			It("accepts even samples when AllowEvenSamples is true", func() {
				cfg := validRequires()
				cfg.SamplesPerModel = 4
				cfg.AllowEvenSamples = true

				err := cfg.Validate()
				Expect(err).NotTo(HaveOccurred())
			})
		})

		DescribeTable("rejects numeric values outside their range",
			func(mutate func(*config.RequiresConfig), field, msg string) {
				cfg := validRequires()
				mutate(&cfg)

				err := cfg.Validate()
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(field))
				Expect(err.Error()).To(ContainSubstring(msg))
			},
			Entry("consensus threshold below range", func(c *config.RequiresConfig) { c.ConsensusThreshold = 0.4 }, "analysis.requires.consensus_threshold", "must be in range [0.5, 1.0]"),
			Entry("consensus threshold above range", func(c *config.RequiresConfig) { c.ConsensusThreshold = 1.1 }, "analysis.requires.consensus_threshold", "must be in range [0.5, 1.0]"),
			Entry("max error rate below range", func(c *config.RequiresConfig) { c.MaxErrorRate = -0.1 }, "analysis.requires.max_error_rate", "must be in range [0.0, 1.0]"),
			Entry("max error rate above range", func(c *config.RequiresConfig) { c.MaxErrorRate = 1.1 }, "analysis.requires.max_error_rate", "must be in range [0.0, 1.0]"),
			Entry("sampling temperature below range", func(c *config.RequiresConfig) { c.SamplingTemperature = -0.1 }, "analysis.requires.sampling_temperature", "must be in range [0.0, 2.0]"),
			Entry("sampling temperature above range", func(c *config.RequiresConfig) { c.SamplingTemperature = 2.1 }, "analysis.requires.sampling_temperature", "must be in range [0.0, 2.0]"),
		)

		DescribeTable("accepts numeric values at and inside range bounds",
			func(mutate func(*config.RequiresConfig)) {
				cfg := validRequires()
				mutate(&cfg)

				Expect(cfg.Validate()).To(Succeed())
			},
			Entry("consensus threshold min valid", func(c *config.RequiresConfig) { c.ConsensusThreshold = 0.5 }),
			Entry("consensus threshold mid valid", func(c *config.RequiresConfig) { c.ConsensusThreshold = 0.7 }),
			Entry("consensus threshold max valid", func(c *config.RequiresConfig) { c.ConsensusThreshold = 1.0 }),
			Entry("max error rate min valid", func(c *config.RequiresConfig) { c.MaxErrorRate = 0.0 }),
			Entry("max error rate mid valid", func(c *config.RequiresConfig) { c.MaxErrorRate = 0.5 }),
			Entry("max error rate max valid", func(c *config.RequiresConfig) { c.MaxErrorRate = 1.0 }),
			Entry("sampling temperature min valid", func(c *config.RequiresConfig) { c.SamplingTemperature = 0.0 }),
			Entry("sampling temperature mid valid", func(c *config.RequiresConfig) { c.SamplingTemperature = 1.0 }),
			Entry("sampling temperature max valid", func(c *config.RequiresConfig) { c.SamplingTemperature = 2.0 }),
		)

		DescribeTable("required fields when enabled",
			func(mutate func(*config.RequiresConfig), field, msg string) {
				cfg := validRequires()
				mutate(&cfg)

				err := cfg.Validate()
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(field))
				Expect(err.Error()).To(ContainSubstring(msg))
			},
			Entry("empty models", func(c *config.RequiresConfig) { c.Models = []string{} }, "analysis.requires.models", "must not be empty"),
			Entry("zero samples per model", func(c *config.RequiresConfig) { c.SamplesPerModel = 0 }, "analysis.requires.samples_per_model", "must be positive"),
			Entry("zero max tokens", func(c *config.RequiresConfig) { c.MaxTokens = 0 }, "analysis.requires.max_tokens", "must be positive"),
			Entry("zero max source chars", func(c *config.RequiresConfig) { c.MaxSourceChars = 0 }, "analysis.requires.max_source_chars", "must be positive"),
			Entry("zero max target chars", func(c *config.RequiresConfig) { c.MaxTargetChars = 0 }, "analysis.requires.max_target_chars", "must be positive"),
		)

		It("skips validation when disabled", func() {
			cfg := config.RequiresConfig{
				Enabled:            false,
				Models:             []string{},
				SamplesPerModel:    0,
				ConsensusThreshold: 2.0,
				MaxErrorRate:       -1.0,
			}

			err := cfg.Validate()
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
