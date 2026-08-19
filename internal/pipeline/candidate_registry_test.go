package pipeline_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("BuildCandidateRegistry", func() {
	It("registers enabled builtin generators and skips disabled ones", func() {
		cfg := config.CandidateConfig{Generators: []config.CandidateGeneratorEntry{
			{Name: "keyword", Enabled: true},
			{Name: "level", Enabled: true},
			{Name: "semantic", Enabled: false},
		}}

		reg, err := pipeline.BuildCandidateRegistry(cfg)
		Expect(err).NotTo(HaveOccurred())

		_, err = reg.Get("keyword")
		Expect(err).NotTo(HaveOccurred())

		_, err = reg.Get("level")
		Expect(err).NotTo(HaveOccurred())

		_, err = reg.Get("semantic")
		Expect(err).To(HaveOccurred())
	})

	It("registers the semantic generator when enabled", func() {
		cfg := config.CandidateConfig{Generators: []config.CandidateGeneratorEntry{
			{Name: "semantic", Enabled: true},
		}}

		reg, err := pipeline.BuildCandidateRegistry(cfg)
		Expect(err).NotTo(HaveOccurred())

		_, err = reg.Get("semantic")
		Expect(err).NotTo(HaveOccurred())
	})

	It("rejects unknown generator names loudly instead of skipping them", func() {
		cfg := config.CandidateConfig{Generators: []config.CandidateGeneratorEntry{
			{Name: "typo-generator", Enabled: true},
		}}

		_, err := pipeline.BuildCandidateRegistry(cfg)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("typo-generator"))
	})

	It("returns an empty registry when no generators are configured", func() {
		cfg := config.CandidateConfig{}

		reg, err := pipeline.BuildCandidateRegistry(cfg)
		Expect(err).NotTo(HaveOccurred())
		Expect(reg.All()).To(BeEmpty())
	})
})
