package config_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("analysis.engine.retry_on_timeout", func() {
	It("defaults to false", func() {
		_, cfg, err := loadConfigFile("pipeline:\n  max_concurrent_jobs: 10\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Analysis.Engine.RetryOnTimeout).To(BeFalse())
		Expect(cfg.Analysis.Engine.ForTenant("acme-corp")).To(Equal(config.EngineTenantConfig{RetryOnTimeout: false}))
	})

	It("lets a tenant override of true win over a global false", func() {
		_, cfg, err := loadConfigFile("analysis:\n  engine:\n    tenant_overrides:\n      acme-corp:\n        retry_on_timeout: true\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Analysis.Engine.ForTenant("acme-corp")).To(Equal(config.EngineTenantConfig{RetryOnTimeout: true}))
	})

	It("lets a tenant override of false win over a global true", func() {
		yes, no := true, false
		e := config.EngineConfig{RetryOnTimeout: yes,
			TenantOverrides: map[string]config.EngineOverride{"acme-corp": {RetryOnTimeout: &no}}}
		Expect(e.ForTenant("acme-corp")).To(Equal(config.EngineTenantConfig{RetryOnTimeout: false}))
	})

	It("uses the global value for a tenant with no override", func() {
		yes := true
		e := config.EngineConfig{RetryOnTimeout: true,
			TenantOverrides: map[string]config.EngineOverride{"acme-corp": {}, "other": {RetryOnTimeout: &yes}}}
		Expect(e.ForTenant("acme-corp")).To(Equal(config.EngineTenantConfig{RetryOnTimeout: true}))
		Expect(e.ForTenant("unlisted")).To(Equal(config.EngineTenantConfig{RetryOnTimeout: true}))
	})

	DescribeTable("rejects an invalid override under its path, saying what is allowed",
		func(key, path string) {
			cfg := validConfig()
			cfg.Analysis.Engine.TenantOverrides = map[string]config.EngineOverride{key: {}}
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(err).To(MatchError(ContainSubstring(tenant.IDRule)))
			expectOnlyFieldError(err, path)
		},
		Entry("a tenant key that is not a valid tenant ID", "Bad Tenant!", "analysis.engine.tenant_overrides.Bad Tenant!"),
	)

	It("rejects an unknown key under an engine override", func() {
		_, _, err := loadConfigFile("analysis:\n  engine:\n    tenant_overrides:\n      acme-corp:\n        retry_on_timout: true\n")
		Expect(err).To(MatchError(config.ErrInvalidConfig))
		Expect(err.Error()).To(ContainSubstring(`"analysis.engine.tenant_overrides.acme-corp.retry_on_timout"`))
	})

	It("rejects a global-only engine key under an engine override, naming it as not overridable", func() {
		_, _, err := loadConfigFile("analysis:\n  engine:\n    tenant_overrides:\n      acme-corp:\n        max_retries: 5\n")
		Expect(err).To(MatchError(config.ErrInvalidConfig))
		Expect(err.Error()).To(ContainSubstring(`"analysis.engine.tenant_overrides.acme-corp.max_retries"`))
	})
})
