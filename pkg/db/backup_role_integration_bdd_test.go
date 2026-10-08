//go:build integration

package db_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
)

// backup_user (migration 006) runs `crosscodexd admin backup`. It may list
// tenant IDs and stream base backups (REPLICATION), and nothing else.
// The suite entry point lives in db_integration_bdd_test.go.
var _ = Describe("backup_user role", Ordered, func() {
	var backupConn *sql.DB
	ctx := context.Background()
	tenants := []string{"bk-alpha", "bk-beta"}

	BeforeAll(func() {
		if suPool == nil {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}
		pw, err := dbtest.NewRolePassword()
		Expect(err).NotTo(HaveOccurred())
		stmt, err := dbtest.AlterRolePasswordSQL("backup_user", pw)
		Expect(err).NotTo(HaveOccurred())
		Expect(suPool.Exec(ctx, stmt)).To(Succeed(), "set backup_user password")

		u, err := url.Parse(suDSN)
		Expect(err).NotTo(HaveOccurred())
		u.User = url.UserPassword("backup_user", pw)
		backupConn, err = sql.Open("pgx", u.String())
		Expect(err).NotTo(HaveOccurred())

		for _, t := range tenants {
			setupTenant(t, "Backup "+t)
		}
	})

	AfterAll(func() {
		if backupConn != nil {
			backupConn.Close()
		}
		if suPool != nil {
			for _, t := range tenants {
				_ = suPool.Exec(ctx, "DELETE FROM tenants WHERE tenant_id = $1", t)
			}
		}
	})

	It("has REPLICATION but not SUPERUSER or BYPASSRLS", func() {
		var repl, super, bypass bool
		Expect(suPool.QueryRow(ctx,
			"SELECT rolreplication, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = 'backup_user'").
			Scan(&repl, &super, &bypass)).To(Succeed())
		Expect([]bool{repl, super, bypass}).To(Equal([]bool{true, false, false}))
	})

	// WAL-G backup-push reads data_directory, which PostgreSQL shows only to
	// superusers and members of pg_read_all_settings.
	It("reads the server settings WAL-G backup-push needs", func() {
		var dataDir string
		Expect(backupConn.QueryRowContext(ctx, "SHOW data_directory").Scan(&dataDir)).To(Succeed())
		Expect(dataDir).NotTo(BeEmpty())
	})

	It("lists every tenant ID without app.current_tenant", func() {
		rows, err := backupConn.QueryContext(ctx, "SELECT tenant_id FROM tenants")
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()
		var got []string
		for rows.Next() {
			var id string
			Expect(rows.Scan(&id)).To(Succeed())
			got = append(got, id)
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		Expect(got).To(ContainElements(tenants))
	})

	DescribeTable("is denied everything else",
		func(query string) {
			_, err := backupConn.ExecContext(ctx, query)
			Expect(err).To(HaveOccurred())
			Expect(db.IsPgErrorCode(err, "42501")).To(BeTrue(), "want insufficient_privilege, got %v", err)
		},
		Entry("other tenants columns", "SELECT display_name FROM tenants"),
		Entry("tenant data tables", "SELECT count(*) FROM jobs"),
		Entry("writing tenants", "INSERT INTO tenants (tenant_id, display_name) VALUES ('bk-evil', 'x')"),
		Entry("deleting tenants", "DELETE FROM tenants WHERE tenant_id = 'bk-alpha'"),
		Entry("disabling RLS", "ALTER TABLE tenants DISABLE ROW LEVEL SECURITY"),
		Entry("changing server settings", "ALTER SYSTEM SET archive_command = 'true'"),
	)

	It("leaves tenants unchanged after the denied writes", func() {
		var n int
		Expect(suPool.QueryRow(ctx,
			"SELECT count(*) FROM tenants WHERE tenant_id = ANY($1)",
			[]string{"bk-alpha", "bk-beta", "bk-evil"}).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(2), fmt.Sprintf("want only %v", tenants))
	})
})
