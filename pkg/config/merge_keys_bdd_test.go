package config_test

import (
	"context"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

// hasReferences reports whether n still holds an alias, an anchor or a "<<"
// merge key anywhere below it.
func hasReferences(n *yaml.Node) bool {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return true
	}
	for i, c := range n.Content {
		if n.Kind == yaml.MappingNode && i%2 == 0 && isMergeKeyNode(c) {
			return true
		}
		if hasReferences(c) {
			return true
		}
	}
	return false
}

// isMergeKeyNode mirrors yaml.v3's isMerge: a "<<" scalar whose tag is
// empty, "!" or !!merge.
func isMergeKeyNode(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Value == "<<" && (n.Tag == "" || n.Tag == "!" || n.ShortTag() == "!!merge")
}

var _ = Describe("YAML merge keys and aliases in a file layer", func() {
	DescribeTable("resolve to the values yaml.v3 decodes, with no reference left",
		func(content string) {
			raw := config.ExportMustParseYAML(content)
			resolved, err := config.ExportResolveReferences(raw, "test.yaml")
			Expect(err).NotTo(HaveOccurred())
			Expect(hasReferences(resolved)).To(BeFalse())

			var want, got any
			Expect(raw.Decode(&want)).To(Succeed())
			Expect(resolved.Decode(&got)).To(Succeed())
			Expect(got).To(Equal(want))
		},
		Entry("a merged mapping", "logging:\n  <<: {level: debug, format: json}\n"),
		Entry("a merged alias", "a: &a {level: debug}\nb:\n  <<: *a\n"),
		Entry("a merged sequence of aliases, earlier entries winning",
			"a: &a {level: debug}\nb: &b {level: info, format: json}\nc:\n  <<: [*a, *b]\n"),
		Entry("an explicit key before the merge key winning", "a: &a {level: debug}\nb:\n  level: error\n  <<: *a\n"),
		Entry("an explicit key after the merge key winning", "a: &a {level: debug}\nb:\n  <<: *a\n  level: error\n"),
		Entry("a merge inside a merged mapping", "a: &a {<<: {format: text}, level: debug}\nb:\n  <<: *a\n  format: json\n"),
		Entry("an alias to a whole section", "a: &a {level: debug, nested: {x: [1, 2]}}\nb: *a\n"),
		Entry("an alias inside a sequence", "a: &a {x: 1}\nb: [*a, *a]\n"),
		Entry("an alias to a scalar", "a: &s debug\nb: {level: *s}\n"),
		Entry("an alias to a sequence as a value", "a: &q [1, 2]\nb: {x: *q}\n"),
		Entry("an inline mapping inside a merge sequence", "a: &a {x: 1}\nb:\n  <<: [*a, {y: 2, x: 3}]\n"),
		Entry("merged values that are aliases", "s: &s v\nq: &q [1]\na: &a {x: *s, y: *q}\nb: {<<: *a}\n"),
		Entry("an alias as a key", "k: &k level\nb: {*k : debug}\n"),
		Entry("an explicit !!merge tag on a merge key", "a: &a {x: 1}\nb: {!!merge <<: *a}\n"),
		Entry("an explicit long-form merge tag", "b: {!<tag:yaml.org,2002:merge> <<: {x: 1}}\n"),
		Entry("a non-specific tag on a merge key", "b: {! <<: {x: 1}}\n"),
		Entry("a !!merge tag on a key that is not <<", "b: {!!merge foo: {p: 1}}\n"),
		Entry("a quoted << key", "b: {\"<<\": {p: 1}}\n"),
	)

	// Decoded into any, a !!binary key makes yaml.v3 build a
	// map[interface{}]interface{}, so these compare as string maps.
	DescribeTable("write a !!binary key as the name yaml.v3 decodes it to",
		func(content string) {
			raw := config.ExportMustParseYAML(content)
			resolved, err := config.ExportResolveReferences(raw, "test.yaml")
			Expect(err).NotTo(HaveOccurred())

			var want, got map[string]map[string]string
			Expect(raw.Decode(&want)).To(Succeed())
			Expect(resolved.Decode(&got)).To(Succeed())
			Expect(got).To(Equal(want))
			Expect(got["b"]).To(HaveKeyWithValue("level", "debug"))
		},
		Entry("on its own", "b:\n  !!binary bGV2ZWw=: debug\n"),
		Entry("winning over a merged key of the same name", "a: &a {level: info}\nb:\n  !!binary bGV2ZWw=: debug\n  <<: *a\n"),
	)

	It("leaves the source node untouched", func() {
		raw := config.ExportMustParseYAML("a: &a {level: debug}\nb:\n  <<: *a\n")
		_, err := config.ExportResolveReferences(raw, "test.yaml")
		Expect(err).NotTo(HaveOccurred())
		Expect(hasReferences(raw)).To(BeTrue())
	})

	DescribeTable("rejects what yaml.v3 refuses to decode, naming the file",
		func(content, wantMsg string) {
			resolved, err := config.ExportResolveReferences(config.ExportMustParseYAML(content), "layer.yaml")
			Expect(err).To(MatchError(ContainSubstring("layer.yaml")))
			Expect(err).To(MatchError(ContainSubstring(wantMsg)))
			Expect(resolved).To(BeNil())
		},
		Entry("a scalar merge value", "logging:\n  <<: debug\n", "map merge requires map"),
		Entry("two merge keys in one mapping", "logging:\n  <<: {level: debug}\n  <<: {format: json}\n", `mapping key "<<" already defined`),
		Entry("an anchor that contains itself", "a: &a [*a]\n", "contains itself"),
	)

	Describe("when loading", func() {
		It("applies a merged mapping over a default the section sets explicitly", func() {
			_, cfg, err := loadConfigFile("logging:\n  <<: {level: debug, format: json}\n")
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Logging.Level).To(Equal("debug"))
			Expect(cfg.Logging.Format).To(Equal("json"))
		})

		It("applies a merged alias over a default", func() {
			_, cfg, err := loadConfigFile("llm:\n  tenant_overrides:\n    acme-corp: &o\n      default_model: m1\n  <<: *o\n")
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.LLM.DefaultModel).To(Equal("m1"))
		})

		It("applies a merged sequence of aliases, the earlier alias winning", func() {
			_, cfg, err := loadConfigFile("llm:\n  tenant_overrides:\n    acme-corp: &a\n      default_model: m1\n" +
				"    beta-inc: &b\n      default_model: m2\n      embedding_model: e2\n  <<: [*a, *b]\n")
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.LLM.DefaultModel).To(Equal("m1"))
			Expect(cfg.LLM.EmbeddingModel).To(Equal("e2"))
		})

		It("keeps an explicit key in the same mapping over the merged value", func() {
			_, cfg, err := loadConfigFile("logging:\n  level: error\n  <<: {level: debug, format: json}\n")
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Logging.Level).To(Equal("error"))
			Expect(cfg.Logging.Format).To(Equal("json"))
		})

		It("lets a later layer's merged value override an earlier layer's explicit one", func() {
			xdg := GinkgoT().TempDir()
			GinkgoT().Setenv("XDG_CONFIG_HOME", xdg)
			writeTestFile(filepath.Join(xdg, "crosscodex", "config.yaml"), "logging:\n  level: info\n")
			writeTestFile(filepath.Join(xdg, "crosscodex", "conf.d", "50-debug.yaml"), "logging:\n  <<: {level: debug}\n")
			cfg, err := config.NewLoader().Load(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Logging.Level).To(Equal("debug"))
		})

		It("decodes a tenant override set through an alias", func() {
			_, cfg, err := loadConfigFile("llm:\n  tenant_overrides:\n    acme-corp: &o\n      default_model: m1\n    beta-inc: *o\n")
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.LLM.TenantOverrides).To(HaveKey("beta-inc"))
			Expect(cfg.LLM.TenantOverrides["beta-inc"].DefaultModel).To(HaveValue(Equal("m1")))
		})
	})
})
