//go:build !integration

package main

import (
	"bytes"
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

// unreachableMigrationDSN points at a port nothing listens on: a spec that
// fails with a validation error rather than a connection error proves
// migrateDatabase refused the config before touching the database.
const unreachableMigrationDSN = "postgres://postgres@127.0.0.1:1/x?connect_timeout=1"

var _ = Describe("runMigrate arg parsing", func() {
	DescribeTable("returns the usage exit code without loading config",
		func(want int, args ...string) {
			Expect(runMigrate(args)).To(Equal(want))
		},
		Entry("-h", 0, "-h"),
		Entry("a positional argument", 2, "now"),
		Entry("an unknown flag", 2, "--force"),
	)
})

var _ = Describe("migrateDatabase config validation", func() {
	DescribeTable("refuses the config before connecting",
		func(mutate func(*config.Config), want string) {
			cfg := &config.Config{Database: config.DatabaseConfig{
				MigrationDSN: unreachableMigrationDSN,
				DSN:          "postgres://app_user:s3cret@127.0.0.1:1/x",
			}}
			mutate(cfg)
			var out bytes.Buffer
			err := migrateDatabase(context.Background(), cfg, &out)
			Expect(err).To(MatchError(ContainSubstring(want)))
			Expect(out.String()).To(BeEmpty(), "nothing may be reported as changed")
			Expect(err.Error()).NotTo(ContainSubstring("s3cr"), "a password must never appear in an error")
		},
		Entry("no migration_dsn",
			func(c *config.Config) { c.Database.MigrationDSN = "" },
			"database.migration_dsn is not set. crosscodexd migrate applies schema migrations as the schema owner; set database.migration_dsn (or CROSSCODEX_DATABASE_MIGRATION_DSN)"),
		Entry("a migration_dsn that does not parse",
			func(c *config.Config) { c.Database.MigrationDSN = "postgres://crosscodex:s3cr%t@127.0.0.1:1/x" },
			"database.migration_dsn is not a valid postgres:// URL; check it for characters that must be percent-encoded (passwords should be hex). The value is not shown because it contains a password"),
		Entry("a keyword/value migration_dsn",
			func(c *config.Config) {
				c.Database.MigrationDSN = "host=127.0.0.1 port=1 user=crosscodex password=s3cret"
			},
			"database.migration_dsn is not a valid postgres:// URL"),
		Entry("a migration_dsn with another scheme",
			func(c *config.Config) { c.Database.MigrationDSN = "mysql://crosscodex:s3cret@127.0.0.1:1/x" },
			"database.migration_dsn is not a valid postgres:// URL"),
		Entry("a migration_dsn without a host",
			func(c *config.Config) { c.Database.MigrationDSN = "postgres://crosscodex:s3cret@/x" },
			"database.migration_dsn is not a valid postgres:// URL"),
		Entry("a migration_dsn the database driver rejects",
			func(c *config.Config) {
				c.Database.MigrationDSN = "postgres://crosscodex:s3cret@127.0.0.1:1/x?sslmode=bogus&connect_timeout=1"
			},
			"create migrator"),
		Entry("database.dsn for the wrong role",
			func(c *config.Config) { c.Database.DSN = "postgres://postgres:s3cret@127.0.0.1:1/x" },
			`database.dsn connects as "postgres", but it must connect as app_user`),
		Entry("database.dsn without a user",
			func(c *config.Config) { c.Database.DSN = "postgres://127.0.0.1:1/x" },
			`database.dsn connects as "", but it must connect as app_user`),
		Entry("database.graph_dsn for the wrong role",
			func(c *config.Config) { c.Database.GraphDSN = "postgres://app_user:s3cret@127.0.0.1:1/x" },
			`database.graph_dsn connects as "app_user", but it must connect as graph_user`),
		Entry("database.purge_dsn for the wrong role",
			func(c *config.Config) { c.Database.PurgeDSN = "postgres://app_user@127.0.0.1:1/x" },
			`database.purge_dsn connects as "app_user", but it must connect as purge_user`),
		Entry("database.tenant_admin_dsn for the wrong role",
			func(c *config.Config) { c.Database.TenantAdminDSN = "postgres://app_user@127.0.0.1:1/x" },
			`database.tenant_admin_dsn connects as "app_user", but it must connect as tenant_admin`),
		Entry("backup.dsn for the wrong role",
			func(c *config.Config) { c.Backup.DSN = "postgres://app_user@127.0.0.1:1/x" },
			`backup.dsn connects as "app_user", but it must connect as backup_user`),
		Entry("a keyword/value DSN",
			func(c *config.Config) { c.Database.DSN = "host=127.0.0.1 port=1 user=app_user password=s3cret" },
			"database.dsn must be a postgres:// URL so crosscodexd migrate can read its role"),
		Entry("a URL that does not parse",
			func(c *config.Config) { c.Database.DSN = "postgres://app_user:s3cret@127.0.0.1:port/x" },
			"database.dsn must be a postgres:// URL so crosscodexd migrate can read its role"),
		Entry("another scheme",
			func(c *config.Config) { c.Database.DSN = "mysql://app_user:s3cret@127.0.0.1:1/x" },
			"database.dsn must be a postgres:// URL so crosscodexd migrate can read its role"),
		Entry("a role DSN with the password in the query",
			func(c *config.Config) { c.Database.DSN = "postgres://app_user@127.0.0.1:1/x?password=s3cret" },
			"database.dsn carries a password query parameter, which crosscodexd migrate cannot use to set app_user's password; put the password in the URL userinfo (postgres://role:password@host/...)"),
		Entry("a role DSN with passwords in both places",
			func(c *config.Config) {
				c.Database.PurgeDSN = "postgres://purge_user:s3cret@127.0.0.1:1/x?sslmode=disable&password=s3cret2"
			},
			"database.purge_dsn carries a password query parameter"),
		Entry("a non-printable password",
			func(c *config.Config) { c.Database.GraphDSN = "postgres://graph_user:with%20space@127.0.0.1:1/x" },
			"database.graph_dsn: scram verifier: the password must be printable ASCII"),
	)
})
