package config_test

import (
	"context"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("CatalogConfig.RequirementPartNames", func() {
	load := func(yaml string) (*config.Config, error) {
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		writeTestFile(path, yaml)
		return config.NewLoader().Load(context.Background(), config.WithConfigPath(path))
	}

	It("defaults to statement", func() {
		cfg, err := config.NewLoader().Load(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Catalog.RequirementPartNames).To(Equal([]string{"statement"}))
	})

	DescribeTable("accepts a valid override",
		func(yaml string, want []string) {
			cfg, err := load(yaml)
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Catalog.RequirementPartNames).To(Equal(want))
		},
		Entry("two names, order kept", "catalog:\n  requirement_part_names: [statement, overview]\n", []string{"statement", "overview"}),
		Entry("an underscore-first name", "catalog:\n  requirement_part_names: [_req.part-1]\n", []string{"_req.part-1"}),
		Entry("a non-ASCII letter", "catalog:\n  requirement_part_names: [exigenceé]\n", []string{"exigenceé"}),
	)

	It("rejects an empty list and says at least one name is required", func() {
		_, err := load("catalog:\n  requirement_part_names: []\n")
		Expect(err).To(MatchError(config.ErrInvalidConfig))
		Expect(err.Error()).To(ContainSubstring("catalog.requirement_part_names"))
		Expect(err.Error()).To(ContainSubstring("at least one name is required"))
		expectOnlyFieldError(err, "catalog.requirement_part_names")
	})

	DescribeTable("rejects a name that is not an OSCAL token and quotes it",
		func(name string) {
			_, err := load("catalog:\n  requirement_part_names: [statement, \"" + name + "\"]\n")
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(err.Error()).To(ContainSubstring("catalog.requirement_part_names[1]"))
			Expect(err.Error()).To(ContainSubstring(`"` + name + `"`))
			Expect(err.Error()).To(ContainSubstring("letter or _"))
			expectOnlyFieldError(err, "catalog.requirement_part_names[1]")
		},
		Entry("contains a space", "bad name"),
		Entry("starts with a digit", "1st"),
		Entry("starts with a hyphen", "-x"),
		Entry("is empty", ""),
	)
})
