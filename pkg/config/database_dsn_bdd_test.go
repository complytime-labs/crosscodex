package config_test

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

func migrationDSN(d config.DatabaseConfig) string   { return d.MigrationDSN }
func tenantAdminDSN(d config.DatabaseConfig) string { return d.TenantAdminDSN }

// Each privileged DSN is empty by default, loads from YAML and from its
// CROSSCODEX_DATABASE_* env var, and never leaks into database.dsn.
var _ = Describe("database privileged DSNs", func() {
	var home string

	BeforeEach(func() {
		home = GinkgoT().TempDir()
		GinkgoT().Setenv("XDG_CONFIG_HOME", home)
	})

	load := func() *config.Config {
		cfg, err := config.NewLoader().Load(context.Background())
		Expect(err).NotTo(HaveOccurred())
		return cfg
	}

	DescribeTable("defaults to empty",
		func(field func(config.DatabaseConfig) string) {
			Expect(field(load().Database)).To(BeEmpty())
		},
		Entry("migration_dsn, so `crosscodexd migrate` refuses until it is set",
			migrationDSN),
		Entry("tenant_admin_dsn, so `admin tenant` refuses until it is set",
			tenantAdminDSN),
	)

	DescribeTable("loads from YAML",
		func(yamlKey, dsn string, field func(config.DatabaseConfig) string) {
			dir := filepath.Join(home, "crosscodex")
			Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(dir, "config.yaml"),
				[]byte("database:\n  "+yamlKey+": \""+dsn+"\"\n"), 0o600)).To(Succeed())
			Expect(field(load().Database)).To(Equal(dsn))
		},
		Entry("migration_dsn", "migration_dsn", "postgres://crosscodex@db:5432/crosscodex",
			migrationDSN),
		Entry("tenant_admin_dsn", "tenant_admin_dsn", "postgres://tenant_admin@db:5432/crosscodex",
			tenantAdminDSN),
	)

	DescribeTable("takes its environment variable and leaves database.dsn empty",
		func(envVar, dsn string, field func(config.DatabaseConfig) string) {
			GinkgoT().Setenv(envVar, dsn)
			cfg := load()
			Expect(field(cfg.Database)).To(Equal(dsn))
			Expect(cfg.Database.DSN).To(BeEmpty(), "the env var must not leak into database.dsn")
		},
		Entry("CROSSCODEX_DATABASE_MIGRATION_DSN", "CROSSCODEX_DATABASE_MIGRATION_DSN", "postgres://crosscodex@db:5432/crosscodex",
			migrationDSN),
		Entry("CROSSCODEX_DATABASE_TENANT_ADMIN_DSN", "CROSSCODEX_DATABASE_TENANT_ADMIN_DSN", "postgres://tenant_admin@db:5432/crosscodex",
			tenantAdminDSN),
	)
})
