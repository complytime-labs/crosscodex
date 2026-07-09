package config_test

import (
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

		DescribeTable("consensus threshold validation",
			func(threshold float64, shouldErr bool) {
				cfg := validRequires()
				cfg.ConsensusThreshold = threshold

				err := cfg.Validate()
				if shouldErr {
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("consensus_threshold"))
				} else {
					Expect(err).NotTo(HaveOccurred())
				}
			},
			Entry("below range", 0.4, true),
			Entry("min valid", 0.5, false),
			Entry("mid valid", 0.7, false),
			Entry("max valid", 1.0, false),
			Entry("above range", 1.1, true),
		)

		DescribeTable("max error rate validation",
			func(errRate float64, shouldErr bool) {
				cfg := validRequires()
				cfg.MaxErrorRate = errRate

				err := cfg.Validate()
				if shouldErr {
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("max_error_rate"))
				} else {
					Expect(err).NotTo(HaveOccurred())
				}
			},
			Entry("below range", -0.1, true),
			Entry("min valid", 0.0, false),
			Entry("mid valid", 0.5, false),
			Entry("max valid", 1.0, false),
			Entry("above range", 1.1, true),
		)

		DescribeTable("required fields when enabled",
			func(cfg config.RequiresConfig, wantErr string) {
				err := cfg.Validate()
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(wantErr))
			},
			Entry("empty models", config.RequiresConfig{
				Enabled:             true,
				Models:              []string{},
				SamplesPerModel:     3,
				ConsensusThreshold:  0.6,
				MaxErrorRate:        0.2,
				MaxSourceChars:      1000,
				MaxTargetChars:      1000,
				SamplingTemperature: 0.7,
				MaxTokens:           500,
			}, "models must not be empty"),
			Entry("zero samples per model", config.RequiresConfig{
				Enabled:             true,
				Models:              []string{"model-a"},
				SamplesPerModel:     0,
				ConsensusThreshold:  0.6,
				MaxErrorRate:        0.2,
				MaxSourceChars:      1000,
				MaxTargetChars:      1000,
				SamplingTemperature: 0.7,
				MaxTokens:           500,
			}, "must be positive"),
			Entry("zero max tokens", config.RequiresConfig{
				Enabled:             true,
				Models:              []string{"model-a"},
				SamplesPerModel:     3,
				ConsensusThreshold:  0.6,
				MaxErrorRate:        0.2,
				MaxSourceChars:      1000,
				MaxTargetChars:      1000,
				SamplingTemperature: 0.7,
				MaxTokens:           0,
			}, "must be positive"),
			Entry("zero max source chars", config.RequiresConfig{
				Enabled:             true,
				Models:              []string{"model-a"},
				SamplesPerModel:     3,
				ConsensusThreshold:  0.6,
				MaxErrorRate:        0.2,
				MaxSourceChars:      0,
				MaxTargetChars:      1000,
				SamplingTemperature: 0.7,
				MaxTokens:           500,
			}, "must be positive"),
			Entry("zero max target chars", config.RequiresConfig{
				Enabled:             true,
				Models:              []string{"model-a"},
				SamplesPerModel:     3,
				ConsensusThreshold:  0.6,
				MaxErrorRate:        0.2,
				MaxSourceChars:      1000,
				MaxTargetChars:      0,
				SamplingTemperature: 0.7,
				MaxTokens:           500,
			}, "must be positive"),
		)

		DescribeTable("sampling temperature validation",
			func(temp float64, shouldErr bool) {
				cfg := validRequires()
				cfg.SamplingTemperature = temp

				err := cfg.Validate()
				if shouldErr {
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("sampling_temperature"))
				} else {
					Expect(err).NotTo(HaveOccurred())
				}
			},
			Entry("below range", -0.1, true),
			Entry("min valid", 0.0, false),
			Entry("mid valid", 1.0, false),
			Entry("max valid", 2.0, false),
			Entry("above range", 2.1, true),
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
