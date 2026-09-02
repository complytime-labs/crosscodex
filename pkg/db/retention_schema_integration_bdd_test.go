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
)

// RetentionSchema specs cover:
//  1. Immutability trigger blocks DELETE of completed jobs as app_user
//     (not a member of retention_purge).
//  2. As purge_user (member of retention_purge), DELETE of a completed job
//     succeeds without any SET LOCAL — role membership is engine-enforced.
//  3. NEGATIVE: app_user cannot escalate via SET ROLE retention_purge.
//  4. retention_holds table is present and its partial unique index on name
//     WHERE released_at IS NULL is enforced.
//
// The suite entry point (RunSpecs) is provided by TestDBIntegrationBDD in
// db_integration_bdd_test.go; do not add another one here.
var _ = Describe("Retention Schema", Ordered, func() {

	var purgeConn *sql.DB

	BeforeAll(func() {
		if suPool == nil {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration")
		}
		// Grant purge_user a known password so we can open a connection as it.
		// Scoped here to avoid touching the shared harness.
		err := suPool.Exec(context.Background(), "ALTER ROLE purge_user WITH PASSWORD 'purgepass'")
		Expect(err).NotTo(HaveOccurred(), "set purge_user password")

		purgeConn, err = sql.Open("pgx", purgeUserDSN())
		Expect(err).NotTo(HaveOccurred(), "open purge_user connection")
	})

	AfterAll(func() {
		if purgeConn != nil {
			purgeConn.Close()
		}
		// Drop the tenants these specs provision. Deleting a tenant fires the
		// tenant_graph_drop trigger, removing the per-tenant graph schema owned
		// by graph_user — otherwise those schemas leak and later block
		// 001.down's DROP ROLE graph_user (2BP01). Best-effort, mirroring the
		// tenant cleanup in db_integration_bdd_test.go.
		if suPool != nil {
			ctx := context.Background()
			for _, tenantID := range []string{"ret-block", "ret-purge"} {
				_ = suPool.Exec(ctx, "DELETE FROM jobs WHERE tenant_id = $1", tenantID)
				_ = suPool.Exec(ctx, "DELETE FROM tenants WHERE tenant_id = $1", tenantID)
			}
		}
	})

	// -----------------------------------------------------------------------
	// Blocked delete (app_user is not a member of retention_purge)
	// -----------------------------------------------------------------------

	Describe("Immutability trigger — completed job delete as app_user", func() {
		It("blocks DELETE of a completed job when the session is not a member of retention_purge", func() {
			setupTenant("ret-block", "Ret Block Corp")
			conn := appUserConn()
			DeferCleanup(func() { cleanupJobs("job-ret-block-1") })

			execAsTenant(conn, "ret-block", func(tx *sql.Tx) {
				_, err := tx.ExecContext(context.Background(),
					"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'completed', 'setup') ON CONFLICT DO NOTHING",
					"job-ret-block-1", "ret-block")
				Expect(err).NotTo(HaveOccurred())
			})

			expectErrorAsTenant(conn, "ret-block", func(tx *sql.Tx) {
				_, err := tx.ExecContext(context.Background(),
					"DELETE FROM jobs WHERE job_id = $1", "job-ret-block-1")
				Expect(err).To(HaveOccurred(), "delete of completed job without retention_purge membership must be blocked")
				Expect(db.IsPgErrorCode(err, "23001")).To(BeTrue(),
					"expected restrict_violation (23001), got: %v", err)
			})
		})
	})

	// -----------------------------------------------------------------------
	// Sanctioned delete (purge_user is a member of retention_purge)
	// -----------------------------------------------------------------------

	Describe("Sanctioned purge — role-based carve-out", func() {
		It("allows DELETE of a completed job when the session is purge_user (member of retention_purge)", func() {
			setupTenant("ret-purge", "Ret Purge Corp")
			conn := appUserConn()
			DeferCleanup(func() { cleanupJobs("job-ret-purge-1") })

			execAsTenant(conn, "ret-purge", func(tx *sql.Tx) {
				_, err := tx.ExecContext(context.Background(),
					"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'completed', 'setup') ON CONFLICT DO NOTHING",
					"job-ret-purge-1", "ret-purge")
				Expect(err).NotTo(HaveOccurred())
			})

			// Delete as purge_user — no SET LOCAL; the trigger's pg_has_role
			// check passes because purge_user is a member of retention_purge.
			execAsTenant(purgeConn, "ret-purge", func(tx *sql.Tx) {
				_, err := tx.ExecContext(context.Background(),
					"DELETE FROM jobs WHERE job_id = $1", "job-ret-purge-1")
				Expect(err).NotTo(HaveOccurred(),
					"purge_user (member of retention_purge) must be allowed to delete completed jobs")
			})

			// Verify the row is gone.
			execAsTenant(conn, "ret-purge", func(tx *sql.Tx) {
				var count int
				Expect(tx.QueryRowContext(context.Background(),
					"SELECT count(*) FROM jobs WHERE job_id = $1", "job-ret-purge-1").Scan(&count)).To(Succeed())
				Expect(count).To(Equal(0), "completed job must be absent after purge")
			})
		})
	})

	// -----------------------------------------------------------------------
	// NEGATIVE: app_user cannot escalate to retention_purge
	// -----------------------------------------------------------------------

	Describe("Privilege escalation — SET ROLE retention_purge as app_user", func() {
		It("fails: app_user is not a member of retention_purge so SET ROLE is rejected by the engine", func() {
			conn := appUserConn()
			ctx := context.Background()
			tx, err := conn.BeginTx(ctx, nil)
			Expect(err).NotTo(HaveOccurred(), "BeginTx for escalation attempt")
			defer tx.Rollback() //nolint:errcheck

			_, err = tx.ExecContext(ctx, "SET ROLE retention_purge")
			Expect(err).To(HaveOccurred(),
				"SET ROLE retention_purge as app_user must be rejected (not a member)")
		})
	})

	// -----------------------------------------------------------------------
	// retention_holds table
	// -----------------------------------------------------------------------

	Describe("retention_holds table", func() {
		It("exists and enforces partial unique index on name WHERE released_at IS NULL", func() {
			ctx := context.Background()

			DeferCleanup(func() {
				_ = suPool.Exec(ctx, "DELETE FROM retention_holds WHERE name = 'hold-test-alpha'")
			})

			// Insert an active hold.
			err := suPool.Exec(ctx,
				`INSERT INTO retention_holds (id, name, created_at, created_by)
				 VALUES (gen_random_uuid(), 'hold-test-alpha', now(), 'test-runner')`)
			Expect(err).NotTo(HaveOccurred(), "insert active retention hold")

			// Duplicate active name must violate the partial unique index.
			err = suPool.Exec(ctx,
				`INSERT INTO retention_holds (id, name, created_at, created_by)
				 VALUES (gen_random_uuid(), 'hold-test-alpha', now(), 'test-runner')`)
			Expect(err).To(HaveOccurred(), "duplicate active hold name must be rejected by partial unique index")
			Expect(db.IsPgErrorCode(err, "23505")).To(BeTrue(),
				"expected unique_violation (23505), got: %v", err)

			// A released hold with the same name is allowed (released_at IS NOT NULL
			// falls outside the partial index predicate).
			err = suPool.Exec(ctx,
				`INSERT INTO retention_holds (id, name, created_at, created_by, released_at, released_by)
				 VALUES (gen_random_uuid(), 'hold-test-alpha', now(), 'test-runner', now(), 'test-runner')`)
			Expect(err).NotTo(HaveOccurred(), "released hold with same name must bypass the partial unique index")
		})
	})
})

// purgeUserDSN builds a DSN for purge_user by swapping credentials in suDSN.
// Scoped here so the shared harness is not modified (Ruling H).
func purgeUserDSN() string {
	u, err := url.Parse(suDSN)
	if err != nil {
		panic(fmt.Sprintf("bad suDSN: %v", err))
	}
	u.User = url.UserPassword("purge_user", "purgepass")
	return u.String()
}
