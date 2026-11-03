package config_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("analysis.result_cache", func() {
	It("defaults to enabled with lookups of 1000 keys", func() {
		_, cfg, err := loadConfigFile("pipeline:\n  max_concurrent_jobs: 10\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Analysis.ResultCache.Enabled).To(BeTrue())
		Expect(cfg.Analysis.ResultCache.LookupBatchSize).To(Equal(1000))
		Expect(cfg.Analysis.ResultCache.ForTenant("acme-corp")).To(Equal(config.ResultCacheTenantConfig{Enabled: true, LookupBatchSize: 1000}))
	})

	It("loads a global value and a tenant override of every field, and another tenant inherits the global value", func() {
		_, cfg, err := loadConfigFile("analysis:\n  result_cache:\n    lookup_batch_size: 200\n" +
			"    tenant_overrides:\n      acme-corp:\n        enabled: false\n        lookup_batch_size: 50\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Analysis.ResultCache.ForTenant("acme-corp")).To(Equal(config.ResultCacheTenantConfig{Enabled: false, LookupBatchSize: 50}))
		Expect(cfg.Analysis.ResultCache.ForTenant("other-tenant")).To(Equal(config.ResultCacheTenantConfig{Enabled: true, LookupBatchSize: 200}))
	})

	It("lets an override of enabled: true win over a global false", func() {
		yes := true
		rc := config.ResultCacheConfig{Enabled: false, LookupBatchSize: 1000,
			TenantOverrides: map[string]config.ResultCacheOverride{"acme-corp": {Enabled: &yes}}}
		Expect(rc.ForTenant("acme-corp")).To(Equal(config.ResultCacheTenantConfig{Enabled: true, LookupBatchSize: 1000}))
	})

	It("inherits the global value of every field the override leaves nil", func() {
		fifty := 50
		no := false
		rc := config.ResultCacheConfig{Enabled: true, LookupBatchSize: 200,
			TenantOverrides: map[string]config.ResultCacheOverride{
				"size-only":    {LookupBatchSize: &fifty},
				"enabled-only": {Enabled: &no},
			}}
		Expect(rc.ForTenant("size-only")).To(Equal(config.ResultCacheTenantConfig{Enabled: true, LookupBatchSize: 50}))
		Expect(rc.ForTenant("enabled-only")).To(Equal(config.ResultCacheTenantConfig{Enabled: false, LookupBatchSize: 200}))
	})

	DescribeTable("accepts a batch size at either boundary",
		func(n int, tenantOverride bool) {
			cfg := validConfig()
			if tenantOverride {
				cfg.Analysis.ResultCache.TenantOverrides = map[string]config.ResultCacheOverride{"acme-corp": {LookupBatchSize: &n}}
			} else {
				cfg.Analysis.ResultCache.LookupBatchSize = n
			}
			Expect(config.ExportValidateConfig(cfg)).To(Succeed())
		},
		Entry("global 1", 1, false),
		Entry("global 10000", 10000, false),
		Entry("override 1", 1, true),
		Entry("override 10000", 10000, true),
	)

	DescribeTable("rejects an invalid value under its path, saying what is allowed",
		func(mutate func(*config.ResultCacheConfig), path, want string) {
			cfg := validConfig()
			mutate(&cfg.Analysis.ResultCache)
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(err).To(MatchError(ContainSubstring(want)))
			expectOnlyFieldError(err, path)
		},
		Entry("a zero batch size", func(c *config.ResultCacheConfig) { c.LookupBatchSize = 0 },
			"analysis.result_cache.lookup_batch_size", "must be between 1 and 10000"),
		Entry("a batch size over 10000", func(c *config.ResultCacheConfig) { c.LookupBatchSize = 10001 },
			"analysis.result_cache.lookup_batch_size", "must be between 1 and 10000"),
		Entry("an override batch size of zero", func(c *config.ResultCacheConfig) {
			zero := 0
			c.TenantOverrides = map[string]config.ResultCacheOverride{"acme-corp": {LookupBatchSize: &zero}}
		}, "analysis.result_cache.tenant_overrides.acme-corp.lookup_batch_size", "must be between 1 and 10000"),
		Entry("an override batch size of zero, saying what the setting is for", func(c *config.ResultCacheConfig) {
			zero := 0
			c.TenantOverrides = map[string]config.ResultCacheOverride{"acme-corp": {LookupBatchSize: &zero}}
		}, "analysis.result_cache.tenant_overrides.acme-corp.lookup_batch_size", "how many cache keys one lookup query reads"),
		Entry("an override batch size over 10000", func(c *config.ResultCacheConfig) {
			n := 10001
			c.TenantOverrides = map[string]config.ResultCacheOverride{"acme-corp": {LookupBatchSize: &n}}
		}, "analysis.result_cache.tenant_overrides.acme-corp.lookup_batch_size", "must be between 1 and 10000"),
		Entry("an invalid tenant ID key", func(c *config.ResultCacheConfig) {
			c.TenantOverrides = map[string]config.ResultCacheOverride{"INVALID": {}}
		}, "analysis.result_cache.tenant_overrides.INVALID", "is not a valid tenant ID"),
	)
})

var _ = Describe("llm.model_revisions", func() {
	It("defaults to none", func() {
		_, cfg, err := loadConfigFile("pipeline:\n  max_concurrent_jobs: 10\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.LLM.ModelRevisions).To(BeEmpty())
	})

	It("loads declared revisions", func() {
		_, cfg, err := loadConfigFile("llm:\n  model_revisions:\n    default: \"2026-10-31\"\n    embed: sha-1a2b\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.LLM.ModelRevisions).To(Equal(map[string]string{"default": "2026-10-31", "embed": "sha-1a2b"}))
	})

	It("is global-only: a tenant override that sets it fails to load, naming the key", func() {
		_, _, err := loadConfigFile("llm:\n  tenant_overrides:\n    acme-corp:\n      model_revisions:\n        default: r1\n")
		Expect(err).To(MatchError(config.ErrInvalidConfig))
		Expect(err).To(MatchError(ContainSubstring("model_revisions")))
	})

	It("accepts a revision of exactly 128 bytes", func() {
		cfg := validConfig()
		cfg.LLM.ModelRevisions = map[string]string{"default": strings.Repeat("r", 128)}
		Expect(config.ExportValidateConfig(cfg)).To(Succeed())
	})

	DescribeTable("rejects an invalid revision under its path, saying what to use",
		func(revisions map[string]string, path, want string) {
			cfg := validConfig()
			cfg.LLM.ModelRevisions = revisions
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(err).To(MatchError(ContainSubstring(want)))
			expectOnlyFieldError(err, path)
		},
		Entry("an empty alias", map[string]string{"": "r1"}, `llm.model_revisions[""]`, "has an empty alias"),
		Entry("a revision over 128 bytes", map[string]string{"default": strings.Repeat("r", 129)},
			"llm.model_revisions.default", "over the 128-byte limit"),
		Entry("a whitespace-only alias", map[string]string{" ": "r1"}, "llm.model_revisions. ", "has an empty alias"),
		Entry("an empty revision", map[string]string{"default": ""},
			"llm.model_revisions.default", "omit the alias"),
		Entry("a revision that is not valid UTF-8", map[string]string{"default": "r\xff1"},
			"llm.model_revisions.default", "contains a non-printable character"),
		Entry("a revision with a bidi control", map[string]string{"default": "r\u202e"},
			"llm.model_revisions.default", "contains a non-printable character"),
		Entry("a non-printable revision", map[string]string{"default": "r1\n"},
			"llm.model_revisions.default", "contains a non-printable character"),
	)
})
