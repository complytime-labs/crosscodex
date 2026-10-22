package config_test

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("database.migration_dsn", func() {
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

	It("defaults to empty, so `crosscodexd migrate` refuses until it is set", func() {
		Expect(load().Database.MigrationDSN).To(BeEmpty())
	})

	It("loads from YAML", func() {
		dir := filepath.Join(home, "crosscodex")
		Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "config.yaml"),
			[]byte("database:\n  migration_dsn: \"postgres://crosscodex@db:5432/crosscodex\"\n"), 0o600)).To(Succeed())
		Expect(load().Database.MigrationDSN).To(Equal("postgres://crosscodex@db:5432/crosscodex"))
	})

	It("takes CROSSCODEX_DATABASE_MIGRATION_DSN", func() {
		GinkgoT().Setenv("CROSSCODEX_DATABASE_MIGRATION_DSN", "postgres://crosscodex@db:5432/crosscodex")
		cfg := load()
		Expect(cfg.Database.MigrationDSN).To(Equal("postgres://crosscodex@db:5432/crosscodex"))
		Expect(cfg.Database.DSN).To(BeEmpty(), "the env var must not leak into database.dsn")
	})
})
