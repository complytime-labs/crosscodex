package config_test

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("AnalyzerSetting", func() {
	DescribeTable("names a production analyzer's setting by its dotted config path",
		func(analyzer, setting, path string) {
			got, ok := config.AnalyzerSetting(analyzer, setting)
			Expect(ok).To(BeTrue())
			Expect(got).To(Equal(path))
		},
		Entry("classify lives under classification", "classify", "max_tokens", "analysis.classification.max_tokens"),
		Entry("embedding", "embedding", "enabled", "analysis.embedding.enabled"),
		Entry("artifacts", "artifacts", "max_tokens", "analysis.artifacts.max_tokens"),
		Entry("requires", "requires", "max_tokens", "analysis.requires.max_tokens"),
		Entry("relationship", "relationship", "max_tokens", "analysis.relationship.max_tokens"),
	)

	It("reports no path for a name that is not a production analyzer", func() {
		_, ok := config.AnalyzerSetting("not-an-analyzer", "enabled")
		Expect(ok).To(BeFalse())
	})

	It("names only max_tokens settings the configuration has", func() {
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		cfg, err := config.NewLoader().Load(context.Background())
		Expect(err).NotTo(HaveOccurred())
		out, err := yaml.Marshal(cfg)
		Expect(err).NotTo(HaveOccurred())
		var doc map[string]any
		Expect(yaml.Unmarshal(out, &doc)).To(Succeed())

		for _, name := range []string{"classify", "artifacts", "requires", "relationship"} {
			path, ok := config.AnalyzerSetting(name, "max_tokens")
			Expect(ok).To(BeTrue())
			node := any(doc)
			for part := range strings.SplitSeq(path, ".") {
				m, isMap := node.(map[string]any)
				Expect(isMap).To(BeTrue(), "%s: %q is not a section", path, part)
				Expect(m).To(HaveKey(part), "%s does not exist in the configuration", path)
				node = m[part]
			}
		}
	})
})
