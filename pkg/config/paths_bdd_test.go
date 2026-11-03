package config_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("Config paths", func() {
	DescribeTable("SplitPath splits a dotted path, with a quoted segment for a key that needs it",
		func(path string, want []string) {
			Expect(config.SplitPath(path)).To(Equal(want))
		},
		Entry("plain keys", "llm.tenant_overrides.acme-corp", []string{"llm", "tenant_overrides", "acme-corp"}),
		Entry("a quoted key with a dot", `llm.tenant_overrides["a.b"].timeout`, []string{"llm", "tenant_overrides", "a.b", "timeout"}),
		Entry("a quoted key first", `["lo.gging"]`, []string{"lo.gging"}),
		Entry("a quoted key with a quote", `tls.targets["x\"y"]`, []string{"tls", "targets", `x"y`}),
		Entry("a quoted key last", `tls.targets["x.y"]`, []string{"tls", "targets", "x.y"}),
	)

	DescribeTable("FormatPath quotes the keys that need it and SplitPath reads it back",
		func(keys []string, want string) {
			Expect(config.FormatPath(keys...)).To(Equal(want))
			Expect(config.SplitPath(want)).To(Equal(keys))
		},
		Entry("plain keys", []string{"logging", "level"}, "logging.level"),
		Entry("a dotted map key", []string{"llm", "tenant_overrides", "a.b", "timeout"}, `llm.tenant_overrides["a.b"].timeout`),
		Entry("a dotted first key", []string{"lo.gging"}, `["lo.gging"]`),
		Entry("an empty key", []string{"tls", "targets", ""}, `tls.targets[""]`),
	)

	DescribeTable("SplitPath rejects a malformed path with ErrInvalidPath",
		func(path, wantMsg string) {
			_, err := config.SplitPath(path)
			Expect(err).To(MatchError(config.ErrInvalidPath))
			Expect(err).To(MatchError(ContainSubstring(wantMsg)))
		},
		Entry("empty", "", "empty"),
		Entry("an empty segment", "a..b", "empty key"),
		Entry("a trailing dot", "a.", "empty key"),
		Entry("an unterminated quote", `a["x`, `["key"]`),
		Entry("an unquoted bracket", `a[x]`, `["key"]`),
		Entry("text after a quoted key", `a["x"]b`, `after key "x"`),
	)

	DescribeTable("a FieldError's path quotes a map key that needs it, and splits back to the keys",
		func(mutate func(*config.Config), path string, keys []string) {
			cfg := validConfig()
			mutate(cfg)
			err := config.ExportValidateConfig(cfg)
			expectOnlyFieldError(err, path)
			Expect(config.SplitPath(path)).To(Equal(keys))
		},
		Entry("an invalid tenant key with a dot", func(c *config.Config) {
			c.LLM.TenantOverrides = map[string]config.LLMOverride{"a.b": {}}
		}, `llm.tenant_overrides["a.b"]`, []string{"llm", "tenant_overrides", "a.b"}),
		Entry("a TLS target name with a dot", func(c *config.Config) {
			c.TLS.Targets = map[string]config.TLSOverride{"x.y": {Mode: "bogus"}}
		}, `tls.targets["x.y"].mode`, []string{"tls", "targets", "x.y", "mode"}),
		Entry("a plain tenant key", func(c *config.Config) {
			c.LLM.TenantOverrides = map[string]config.LLMOverride{"BAD": {}}
		}, "llm.tenant_overrides.BAD", []string{"llm", "tenant_overrides", "BAD"}),
	)

	It("quotes an unknown key that contains a dot", func() {
		_, _, err := loadConfigFile("\"lo.gging\":\n  level: debug\n")
		Expect(err).To(MatchError(config.ErrUnknownKey))
		fields := config.FieldErrors(err)
		Expect(fields).To(HaveLen(1))
		Expect(fields[0].Path).To(Equal(`["lo.gging"]`))
	})
})
