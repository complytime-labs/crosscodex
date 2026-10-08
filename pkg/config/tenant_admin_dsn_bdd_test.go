package config_test

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("database.tenant_admin_dsn", func() {
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

	It("defaults to empty, so `admin tenant` refuses until it is set", func() {
		Expect(load().Database.TenantAdminDSN).To(BeEmpty())
	})

	It("loads from YAML", func() {
		dir := filepath.Join(home, "crosscodex")
		Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "config.yaml"),
			[]byte("database:\n  tenant_admin_dsn: \"postgres://tenant_admin@db:5432/crosscodex\"\n"), 0o600)).To(Succeed())
		Expect(load().Database.TenantAdminDSN).To(Equal("postgres://tenant_admin@db:5432/crosscodex"))
	})

	It("takes CROSSCODEX_DATABASE_TENANT_ADMIN_DSN", func() {
		GinkgoT().Setenv("CROSSCODEX_DATABASE_TENANT_ADMIN_DSN", "postgres://tenant_admin@db:5432/crosscodex")
		cfg := load()
		Expect(cfg.Database.TenantAdminDSN).To(Equal("postgres://tenant_admin@db:5432/crosscodex"))
		Expect(cfg.Database.DSN).To(BeEmpty(), "the env var must not leak into database.dsn")
	})
})
