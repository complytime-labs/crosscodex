//go:build integration

package db_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
)

// roleCheckTx opens a superuser transaction that is rolled back after the
// spec, so SET LOCAL ROLE, CREATE ROLE and UPDATE schema_migrations made
// inside it never reach other specs.
func roleCheckTx() (context.Context, *sql.Tx) {
	ctx := context.Background()
	conn, err := sql.Open("pgx", suDSN)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { conn.Close() })
	tx, err := conn.BeginTx(ctx, nil)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { tx.Rollback() }) //nolint:errcheck
	return ctx, tx
}

func mustExec(ctx context.Context, tx *sql.Tx, stmt string) {
	_, err := tx.ExecContext(ctx, stmt)
	Expect(err).NotTo(HaveOccurred(), stmt)
}

var _ = Describe("Schema version and restricted role checks", func() {
	It("LatestMigrationVersion returns the highest embedded migration", func() {
		Expect(db.LatestMigrationVersion()).To(Equal(uint(1)))
	})

	Describe("CheckSchemaVersion", func() {
		DescribeTable("accepts a migrated database",
			func(role string) {
				ctx, tx := roleCheckTx()
				if role != "" {
					mustExec(ctx, tx, "SET LOCAL ROLE "+role)
				}
				Expect(db.CheckSchemaVersion(ctx, tx)).To(Succeed())
			},
			Entry("as app_user", "app_user"),
			Entry("as the superuser", ""),
		)

		It("tells the operator to migrate when the schema is behind", func() {
			ctx, tx := roleCheckTx()
			latest, err := db.LatestMigrationVersion()
			Expect(err).NotTo(HaveOccurred())
			mustExec(ctx, tx, fmt.Sprintf("UPDATE schema_migrations SET version = %d", latest-1))
			Expect(db.CheckSchemaVersion(ctx, tx)).To(MatchError(fmt.Sprintf(
				"the database schema is at version %d but this crosscodexd needs version %d. Run `crosscodexd migrate` with database.migration_dsn set to the schema owner, then start crosscodexd again",
				latest-1, latest)))
		})

		// A full `migrate down` deletes the version row rather than writing
		// version 0, so this is what a database without the schema looks like.
		It("tells the operator to migrate when no migration is recorded", func() {
			ctx, tx := roleCheckTx()
			mustExec(ctx, tx, "DELETE FROM schema_migrations")
			mustExec(ctx, tx, "SET LOCAL ROLE app_user")
			err := db.CheckSchemaVersion(ctx, tx)
			Expect(err).To(MatchError(ContainSubstring("cannot read the schema version")))
			Expect(err).To(MatchError(ContainSubstring("Run `crosscodexd migrate` with database.migration_dsn set to the schema owner")))
		})

		It("tells the operator to upgrade when the schema is ahead", func() {
			ctx, tx := roleCheckTx()
			latest, err := db.LatestMigrationVersion()
			Expect(err).NotTo(HaveOccurred())
			mustExec(ctx, tx, fmt.Sprintf("UPDATE schema_migrations SET version = %d", latest+1))
			err = db.CheckSchemaVersion(ctx, tx)
			Expect(err).To(MatchError(ContainSubstring("newer than this crosscodexd")))
			Expect(err).To(MatchError(ContainSubstring("Upgrade crosscodexd")))
		})

		It("refuses a dirty schema", func() {
			ctx, tx := roleCheckTx()
			mustExec(ctx, tx, "UPDATE schema_migrations SET dirty = true")
			err := db.CheckSchemaVersion(ctx, tx)
			Expect(errors.Is(err, db.ErrMigrationDirty)).To(BeTrue(), "got: %v", err)
			Expect(err).To(MatchError(ContainSubstring("dirty")))
		})

		It("points at crosscodexd migrate when the role cannot read the version", func() {
			ctx, tx := roleCheckTx()
			mustExec(ctx, tx, "SET LOCAL ROLE graph_user")
			err := db.CheckSchemaVersion(ctx, tx)
			Expect(err).To(MatchError(ContainSubstring("cannot read the schema version")))
			Expect(err).To(MatchError(ContainSubstring("crosscodexd migrate")))
		})
	})

	Describe("CheckRestrictedRole", func() {
		DescribeTable("accepts the restricted roles",
			func(role string) {
				ctx, tx := roleCheckTx()
				mustExec(ctx, tx, "SET LOCAL ROLE "+role)
				Expect(db.CheckRestrictedRole(ctx, tx, "database.dsn")).To(Succeed())
			},
			Entry("app_user", "app_user"),
			Entry("graph_user", "graph_user"),
			Entry("purge_user, a member of retention_purge", "purge_user"),
		)

		DescribeTable("refuses a superuser and names the field and the role it must connect as",
			func(field, role string) {
				ctx, tx := roleCheckTx()
				err := db.CheckRestrictedRole(ctx, tx, field)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(And(
					ContainSubstring(field+" connects as"),
					ContainSubstring("is a superuser"),
					ContainSubstring("tenant isolation would not apply"),
					ContainSubstring("connect "+field+" as "+role),
					ContainSubstring("crosscodexd migrate"),
				))
			},
			Entry("database.dsn", "database.dsn", "app_user"),
			Entry("database.graph_dsn", "database.graph_dsn", "graph_user"),
			Entry("database.purge_dsn", "database.purge_dsn", "purge_user"),
		)

		DescribeTable("refuses other RLS-bypassing roles",
			func(setup []string, role, want string) {
				ctx, tx := roleCheckTx()
				for _, stmt := range setup {
					mustExec(ctx, tx, stmt)
				}
				mustExec(ctx, tx, "SET LOCAL ROLE "+role)
				err := db.CheckRestrictedRole(ctx, tx, "database.graph_dsn")
				Expect(err).To(MatchError(ContainSubstring("database.graph_dsn")))
				Expect(err).To(MatchError(ContainSubstring(want)))
			},
			Entry("a BYPASSRLS role",
				[]string{"CREATE ROLE cc_bypass_test BYPASSRLS"}, "cc_bypass_test", "bypass"),
			Entry("the owner of a public table",
				[]string{
					"CREATE ROLE cc_owner_test",
					"CREATE TABLE public.cc_owner_probe()",
					"ALTER TABLE public.cc_owner_probe OWNER TO cc_owner_test",
				}, "cc_owner_test", "owns"),
			Entry("a member of the owner of a public table",
				[]string{
					"CREATE ROLE cc_owner2_test",
					"CREATE TABLE public.cc_owner2_probe()",
					"ALTER TABLE public.cc_owner2_probe OWNER TO cc_owner2_test",
					"CREATE ROLE cc_member_owner_test",
					"GRANT cc_owner2_test TO cc_member_owner_test",
				}, "cc_member_owner_test", "owns"),
			Entry("a member of a superuser role",
				[]string{
					"CREATE ROLE cc_super_test SUPERUSER",
					"CREATE ROLE cc_member_super_test",
					"GRANT cc_super_test TO cc_member_super_test",
				}, "cc_member_super_test", "member of a superuser"),
			Entry("a member of a BYPASSRLS role",
				[]string{
					"CREATE ROLE cc_bypass2_test BYPASSRLS",
					"CREATE ROLE cc_member_bypass_test",
					"GRANT cc_bypass2_test TO cc_member_bypass_test",
				}, "cc_member_bypass_test", "member of a superuser or BYPASSRLS"),
		)
	})
})
