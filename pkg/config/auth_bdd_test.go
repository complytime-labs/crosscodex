package config_test

import (
	"context"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func mapping(tenantID string, match config.X509MatchConfig, roles ...string) config.X509MappingConfig {
	return config.X509MappingConfig{Match: match, Tenant: tenantID, Roles: roles}
}

var _ = Describe("auth.x509_mappings and tls.service_identities", func() {
	It("defaults both to empty, so a shipped config without mappings still loads", func() {
		cfg, err := config.NewLoader().Load(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Auth.X509Mappings).To(BeEmpty())
		Expect(cfg.TLS.ServiceIdentities).To(BeEmpty())
	})

	It("lists service_identities in the compiled defaults", func() {
		Expect(config.ExportDefaultConfigYAML()).To(ContainSubstring("service_identities: []"))
	})

	DescribeTable("accepts valid mappings and identities",
		func(mutate func(*config.Config)) {
			cfg := validConfig()
			mutate(cfg)
			Expect(config.ExportValidateConfig(cfg)).To(Succeed())
		},
		Entry("a single-tenant mapping with an empty tenant", func(c *config.Config) {
			c.Auth.X509Mappings = []config.X509MappingConfig{mapping("", config.X509MatchConfig{CN: "ops-*"}, "admin")}
		}),
		Entry("a single-tenant mapping naming the default tenant", func(c *config.Config) {
			c.Tenants.DefaultTenant = "acme"
			c.Auth.X509Mappings = []config.X509MappingConfig{mapping("acme", config.X509MatchConfig{CN: "ops-*"}, "admin")}
		}),
		Entry("multi-tenant mappings for different tenants", func(c *config.Config) {
			c.Tenants.Enabled = true
			c.Auth.X509Mappings = []config.X509MappingConfig{
				mapping("acme", config.X509MatchConfig{CN: "ops-*", Organization: "Acme"}, "admin", "reviewer"),
				mapping("beta-inc", config.X509MatchConfig{SANURI: "spiffe://beta/*"}, "reviewer"),
			}
		}),
		Entry("mappings whose match blocks differ in one field", func(c *config.Config) {
			c.Auth.X509Mappings = []config.X509MappingConfig{
				mapping("", config.X509MatchConfig{CN: "a"}, "admin"),
				mapping("", config.X509MatchConfig{CN: "a", OrgUnit: "b"}, "admin"),
			}
		}),
		Entry("service identities with glob patterns", func(c *config.Config) {
			c.TLS.ServiceIdentities = []config.X509MatchConfig{{CN: "crosscodex-[a-z]*"}, {SANDNS: "*.svc.cluster.local"}}
		}),
	)

	DescribeTable("rejects each rule under its exact path with an actionable message",
		func(mutate func(*config.Config), path string, wantParts ...string) {
			cfg := validConfig()
			mutate(cfg)
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			expectOnlyFieldError(err, path)
			for _, part := range wantParts {
				Expect(err.Error()).To(ContainSubstring(part))
			}
		},
		Entry("a mapping with an empty match", func(c *config.Config) {
			c.Auth.X509Mappings = []config.X509MappingConfig{
				mapping("", config.X509MatchConfig{CN: "ok"}, "admin"),
				mapping("", config.X509MatchConfig{}, "admin"),
			}
		}, "auth.x509_mappings[1].match", "matches every certificate", "cn"),
		Entry("a mapping with no roles", func(c *config.Config) {
			c.Auth.X509Mappings = []config.X509MappingConfig{mapping("", config.X509MatchConfig{CN: "ok"})}
		}, "auth.x509_mappings[0].roles", "no roles", "admin"),
		Entry("an invalid tenant ID", func(c *config.Config) {
			c.Tenants.Enabled = true
			c.Auth.X509Mappings = []config.X509MappingConfig{mapping("Acme", config.X509MatchConfig{CN: "ok"}, "admin")}
		}, "auth.x509_mappings[0].tenant", `"Acme"`, tenant.IDRule),
		Entry("a missing tenant when tenants.enabled", func(c *config.Config) {
			c.Tenants.Enabled = true
			c.Auth.X509Mappings = []config.X509MappingConfig{mapping("", config.X509MatchConfig{CN: "ok"}, "admin")}
		}, "auth.x509_mappings[0].tenant", "tenants.enabled", "required"),
		Entry("a tenant other than the default when tenants.enabled is false", func(c *config.Config) {
			c.Tenants.DefaultTenant = "acme"
			c.Auth.X509Mappings = []config.X509MappingConfig{mapping("beta-inc", config.X509MatchConfig{CN: "ok"}, "admin")}
		}, "auth.x509_mappings[0].tenant", `"beta-inc"`, "tenants.default_tenant", "tenants.enabled"),
		Entry("a tenant when tenants.enabled is false and no default is set", func(c *config.Config) {
			c.Auth.X509Mappings = []config.X509MappingConfig{mapping("beta-inc", config.X509MatchConfig{CN: "ok"}, "admin")}
		}, "auth.x509_mappings[0].tenant", `"beta-inc"`, "leave it empty"),
		Entry("a duplicate match block", func(c *config.Config) {
			m := config.X509MatchConfig{CN: "ops", Organization: "Acme"}
			c.Auth.X509Mappings = []config.X509MappingConfig{
				mapping("", m, "admin"),
				mapping("", config.X509MatchConfig{CN: "other"}, "admin"),
				mapping("", m, "reviewer"),
			}
		}, "auth.x509_mappings[2].match", "unreachable", "mapping 0"),
		Entry("a malformed glob in a mapping", func(c *config.Config) {
			c.Auth.X509Mappings = []config.X509MappingConfig{mapping("", config.X509MatchConfig{SANEmail: "["}, "admin")}
		}, "auth.x509_mappings[0].match.san_email", `"["`, "never match"),
		Entry("an empty service identity", func(c *config.Config) {
			c.TLS.ServiceIdentities = []config.X509MatchConfig{{CN: "ok"}, {}}
		}, "tls.service_identities[1]", "matches every certificate"),
		Entry("a malformed glob in a service identity", func(c *config.Config) {
			c.TLS.ServiceIdentities = []config.X509MatchConfig{{CN: "ok"}, {OrgUnit: "a[b"}}
		}, "tls.service_identities[1].org_unit", `"a[b"`, "never match"),
	)

	It("reports every bad mapping in one load", func() {
		cfg := validConfig()
		cfg.Auth.X509Mappings = []config.X509MappingConfig{
			mapping("", config.X509MatchConfig{}, "admin"),
			mapping("", config.X509MatchConfig{CN: "ok"}),
		}
		fields := config.FieldErrors(config.ExportValidateConfig(cfg))
		paths := make([]string, 0, len(fields))
		for _, f := range fields {
			paths = append(paths, f.Path)
		}
		Expect(paths).To(ConsistOf("auth.x509_mappings[0].match", "auth.x509_mappings[1].roles"))
	})

	It("rejects a mapping with no roles when loaded from a file, naming the path", func() {
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		writeTestFile(path, "auth:\n  x509_mappings:\n    - match: {cn: ops}\n      roles: []\n")
		_, err := config.NewLoader().Load(context.Background(), config.WithConfigPath(path))
		Expect(err).To(MatchError(config.ErrInvalidConfig))
		expectOnlyFieldError(err, "auth.x509_mappings[0].roles")
	})
})
