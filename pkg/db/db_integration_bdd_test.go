//go:build integration

package db_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func TestDBIntegrationBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "DB Integration Suite")
}

// Redirect slog output to GinkgoWriter so log noise only appears on failure.
var _ = BeforeEach(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

// ---------------------------------------------------------------------------
// Package-level state initialised by SynchronizedBeforeSuite
// ---------------------------------------------------------------------------

// suDSN is the superuser connection string.
var suDSN string

// suPool is the superuser pool for test setup operations.
var suPool db.Pool

var _ = SynchronizedBeforeSuite(func() []byte {
	// Runs on node 1 only: migrate + set role passwords.
	dsn := os.Getenv("TEST_DATABASE_DSN")
	if dsn == "" {
		Fail("TEST_DATABASE_DSN not set — run: task test:integration")
	}

	ctx := context.Background()

	// Run migrations as superuser.
	migrator, err := db.NewMigrator(dsn)
	Expect(err).NotTo(HaveOccurred(), "failed to create migrator")
	Expect(migrator.Up(ctx)).To(Succeed(), "failed to run migrations")
	Expect(migrator.Close()).To(Succeed(), "failed to close migrator")

	// Set passwords for application roles.
	adminDB, err := sql.Open("pgx", dsn)
	Expect(err).NotTo(HaveOccurred(), "failed to open admin connection")
	_, err = adminDB.ExecContext(ctx, "ALTER ROLE app_user WITH PASSWORD 'apppass'")
	Expect(err).NotTo(HaveOccurred(), "failed to set app_user password")
	_, err = adminDB.ExecContext(ctx, "ALTER ROLE graph_user WITH PASSWORD 'graphpass'")
	Expect(err).NotTo(HaveOccurred(), "failed to set graph_user password")
	Expect(adminDB.Close()).To(Succeed(), "failed to close admin connection")

	return []byte(dsn)
}, func(data []byte) {
	// Runs on all nodes: store DSN and create superuser pool.
	suDSN = string(data)

	var err error
	suPool, err = db.NewPool(db.PoolConfig{
		DSN:          suDSN,
		MaxOpenConns: 5,
		Extensions:   []string{"age", "vector"},
	})
	Expect(err).NotTo(HaveOccurred(), "failed to create superuser pool")
})

var _ = AfterSuite(func() {
	if suPool != nil {
		suPool.Close()
	}
})

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func appUserDSN() string {
	u, err := url.Parse(suDSN)
	if err != nil {
		panic(fmt.Sprintf("bad suDSN: %v", err))
	}
	u.User = url.UserPassword("app_user", "apppass")
	return u.String()
}

func appUserConn() *sql.DB {
	conn, err := sql.Open("pgx", appUserDSN())
	Expect(err).NotTo(HaveOccurred(), "failed to open app_user connection")
	DeferCleanup(func() { conn.Close() })
	return conn
}

func graphUserDSN() string {
	u, err := url.Parse(suDSN)
	if err != nil {
		panic(fmt.Sprintf("bad suDSN: %v", err))
	}
	u.User = url.UserPassword("graph_user", "graphpass")
	return u.String()
}

func graphUserConn() *sql.DB {
	conn, err := sql.Open("pgx", graphUserDSN())
	Expect(err).NotTo(HaveOccurred(), "failed to open graph_user connection")
	DeferCleanup(func() { conn.Close() })
	return conn
}

func setupTenant(tenantID, displayName string) {
	ctx := context.Background()
	err := suPool.Exec(ctx,
		"INSERT INTO tenants (tenant_id, display_name) VALUES ($1, $2) ON CONFLICT DO NOTHING",
		tenantID, displayName)
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("setupTenant(%q)", tenantID))
}

func execAsTenant(conn *sql.DB, tenantID string, fn func(tx *sql.Tx)) {
	ctx := context.Background()
	tx, err := conn.BeginTx(ctx, nil)
	Expect(err).NotTo(HaveOccurred(), "BeginTx")
	defer tx.Rollback() //nolint:errcheck

	_, err = tx.ExecContext(ctx, "SELECT set_config('app.current_tenant', $1, true)", tenantID)
	Expect(err).NotTo(HaveOccurred(), "set_config tenant")
	fn(tx)
	Expect(tx.Commit()).To(Succeed(), "Commit")
}

func execAsTenantUser(conn *sql.DB, tenantID, userID string, fn func(tx *sql.Tx)) {
	ctx := context.Background()
	tx, err := conn.BeginTx(ctx, nil)
	Expect(err).NotTo(HaveOccurred(), "BeginTx")
	defer tx.Rollback() //nolint:errcheck

	_, err = tx.ExecContext(ctx, "SELECT set_config('app.current_tenant', $1, true)", tenantID)
	Expect(err).NotTo(HaveOccurred(), "set_config tenant")
	_, err = tx.ExecContext(ctx, "SELECT set_config('app.current_user', $1, true)", userID)
	Expect(err).NotTo(HaveOccurred(), "set_config user")
	fn(tx)
	Expect(tx.Commit()).To(Succeed(), "Commit")
}

func testID(prefix string) string {
	b := make([]byte, 4)
	_, err := rand.Read(b)
	Expect(err).NotTo(HaveOccurred(), "testID rand")
	return fmt.Sprintf("%s-%x", prefix, b)
}

func cleanupJobs(jobIDs ...string) {
	if len(jobIDs) == 0 {
		return
	}
	ctx := context.Background()
	tx, err := suPool.Begin(ctx)
	if err != nil {
		GinkgoWriter.Printf("cleanupJobs begin: %v\n", err)
		return
	}
	defer tx.Rollback() //nolint:errcheck

	if err := tx.Exec(ctx, "ALTER TABLE jobs DISABLE TRIGGER jobs_immutable_update"); err != nil {
		GinkgoWriter.Printf("cleanupJobs disable update trigger: %v\n", err)
		return
	}
	if err := tx.Exec(ctx, "ALTER TABLE jobs DISABLE TRIGGER jobs_immutable_delete"); err != nil {
		GinkgoWriter.Printf("cleanupJobs disable delete trigger: %v\n", err)
		return
	}
	for _, id := range jobIDs {
		if err := tx.Exec(ctx, "DELETE FROM jobs WHERE job_id = $1", id); err != nil {
			GinkgoWriter.Printf("cleanupJobs delete %q: %v\n", id, err)
			return
		}
	}
	if err := tx.Exec(ctx, "ALTER TABLE jobs ENABLE TRIGGER jobs_immutable_update"); err != nil {
		GinkgoWriter.Printf("cleanupJobs enable update trigger: %v\n", err)
		return
	}
	if err := tx.Exec(ctx, "ALTER TABLE jobs ENABLE TRIGGER jobs_immutable_delete"); err != nil {
		GinkgoWriter.Printf("cleanupJobs enable delete trigger: %v\n", err)
		return
	}
	if err := tx.Commit(); err != nil {
		GinkgoWriter.Printf("cleanupJobs commit: %v\n", err)
	}
}

// expectErrorAsTenant runs fn inside a transaction with app.current_tenant
// set, then rolls back. Use this instead of execAsTenant when the operation
// inside fn is expected to fail.
func expectErrorAsTenant(conn *sql.DB, tenantID string, fn func(tx *sql.Tx)) {
	ctx := context.Background()
	tx, err := conn.BeginTx(ctx, nil)
	Expect(err).NotTo(HaveOccurred(), "BeginTx")
	defer tx.Rollback() //nolint:errcheck

	_, err = tx.ExecContext(ctx, "SELECT set_config('app.current_tenant', $1, true)", tenantID)
	Expect(err).NotTo(HaveOccurred(), "set_config tenant")
	fn(tx)
}

// ---------------------------------------------------------------------------
// Integration Tests
// ---------------------------------------------------------------------------

var _ = Describe("Database Integration", Ordered, func() {

	// ===================================================================
	// Connection Pool
	// ===================================================================

	Describe("Connection Pool", func() {
		It("connects and executes a simple query", func() {
			pool, err := db.NewPool(db.PoolConfig{
				DSN:          suDSN,
				MaxOpenConns: 2,
			})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(pool.Close)

			ctx := context.Background()
			row := pool.QueryRow(ctx, "SELECT 1")
			var got int
			Expect(row.Scan(&got)).To(Succeed())
			Expect(got).To(Equal(1))
		})

		It("reports healthy status", func() {
			pool, err := db.NewPool(db.PoolConfig{
				DSN:          suDSN,
				MaxOpenConns: 5,
			})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(pool.Close)

			ctx := context.Background()
			hs, err := pool.Health(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(hs.Connected).To(BeTrue())
			Expect(hs.MaxOpen).To(BeNumerically(">", 0))
		})

		It("verifies required extensions are present", func() {
			pool, err := db.NewPool(db.PoolConfig{
				DSN:          suDSN,
				MaxOpenConns: 2,
				Extensions:   []string{"age", "vector"},
			})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(pool.Close)

			Expect(pool.VerifyExtensions(context.Background())).To(Succeed())
		})

		It("reports missing extensions via ExtensionError", func() {
			pool, err := db.NewPool(db.PoolConfig{
				DSN:          suDSN,
				MaxOpenConns: 2,
				Extensions:   []string{"age", "nonexistent_ext_xyz"},
			})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(pool.Close)

			err = pool.VerifyExtensions(context.Background())
			Expect(err).To(HaveOccurred())

			var extErr *db.ExtensionError
			Expect(errors.As(err, &extErr)).To(BeTrue())
			Expect(extErr.Missing).To(ContainElement("nonexistent_ext_xyz"))
		})

		It("supports idempotent close", func() {
			pool, err := db.NewPool(db.PoolConfig{
				DSN:          suDSN,
				MaxOpenConns: 2,
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(pool.Close()).To(Succeed())
			// Second close must not panic.
			Expect(pool.Close()).To(Succeed())
		})
	})

	// ===================================================================
	// Migrations
	// ===================================================================

	Describe("Migrations", func() {
		It("reports the current migration version", func() {
			migrator, err := db.NewMigrator(suDSN)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(migrator.Close)

			version, dirty, err := migrator.Version(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(version).To(Equal(uint(11)))
			Expect(dirty).To(BeFalse())
		})

		It("runs Up idempotently", func() {
			migrator, err := db.NewMigrator(suDSN)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(migrator.Close)

			Expect(migrator.Up(context.Background())).To(Succeed())
		})
	})

	// ===================================================================
	// Tenant Isolation (RLS)
	// ===================================================================

	Describe("Tenant Isolation (RLS)", func() {
		Context("when two tenants exist", func() {
			BeforeEach(func() {
				setupTenant("iso-alpha", "Alpha Corp")
				setupTenant("iso-beta", "Beta Inc")
			})

			It("isolates jobs between tenants", func() {
				conn := appUserConn()

				// Insert a job for alpha.
				execAsTenant(conn, "iso-alpha", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'setup') ON CONFLICT DO NOTHING",
						"job-iso-a1", "iso-alpha")
					Expect(err).NotTo(HaveOccurred())
				})

				// Insert a job for beta.
				execAsTenant(conn, "iso-beta", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'setup') ON CONFLICT DO NOTHING",
						"job-iso-b1", "iso-beta")
					Expect(err).NotTo(HaveOccurred())
				})

				// Query as alpha: should see only alpha's job.
				execAsTenant(conn, "iso-alpha", func(tx *sql.Tx) {
					rows, err := tx.QueryContext(context.Background(), "SELECT job_id FROM jobs")
					Expect(err).NotTo(HaveOccurred())
					defer rows.Close()

					var ids []string
					for rows.Next() {
						var id string
						Expect(rows.Scan(&id)).To(Succeed())
						ids = append(ids, id)
					}
					Expect(rows.Err()).NotTo(HaveOccurred())
					for _, id := range ids {
						Expect(id).To(HavePrefix("job-iso-a"))
					}
					Expect(ids).NotTo(BeEmpty(), "alpha tenant sees zero jobs")
				})
			})
		})

		Context("when no tenant context is set", func() {
			It("returns zero rows (fail closed)", func() {
				setupTenant("iso-nocontext", "NoCtx Corp")
				conn := appUserConn()

				// Insert a job as the tenant.
				execAsTenant(conn, "iso-nocontext", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'setup') ON CONFLICT DO NOTHING",
						"job-nocontext-1", "iso-nocontext")
					Expect(err).NotTo(HaveOccurred())
				})

				// Query without setting tenant.
				ctx := context.Background()
				tx, err := conn.BeginTx(ctx, nil)
				Expect(err).NotTo(HaveOccurred())
				defer tx.Rollback() //nolint:errcheck

				var count int
				Expect(tx.QueryRowContext(ctx, "SELECT count(*) FROM jobs").Scan(&count)).To(Succeed())
				Expect(count).To(Equal(0))
			})
		})

		Context("when attempting cross-tenant writes", func() {
			It("blocks cross-tenant INSERT via RLS WITH CHECK", func() {
				setupTenant("iso-cross-src", "Source Corp")
				setupTenant("iso-cross-dst", "Dest Corp")
				conn := appUserConn()

				expectErrorAsTenant(conn, "iso-cross-src", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'attacker')",
						"job-cross-write-1", "iso-cross-dst")
					Expect(err).To(HaveOccurred(), "cross-tenant write should be blocked by RLS WITH CHECK")
				})

				// Verify no data was written for the destination tenant.
				execAsTenant(conn, "iso-cross-dst", func(tx *sql.Tx) {
					var count int
					Expect(tx.QueryRowContext(context.Background(),
						"SELECT count(*) FROM jobs WHERE job_id = $1", "job-cross-write-1").Scan(&count)).To(Succeed())
					Expect(count).To(Equal(0), "cross-tenant write leaked data")
				})
			})
		})

		Context("when tenant ID is empty string", func() {
			It("matches zero rows", func() {
				setupTenant("iso-empty", "Empty Corp")
				conn := appUserConn()

				execAsTenant(conn, "iso-empty", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'setup') ON CONFLICT DO NOTHING",
						"job-empty-1", "iso-empty")
					Expect(err).NotTo(HaveOccurred())
				})

				execAsTenant(conn, "", func(tx *sql.Tx) {
					var count int
					Expect(tx.QueryRowContext(context.Background(), "SELECT count(*) FROM jobs").Scan(&count)).To(Succeed())
					Expect(count).To(Equal(0))
				})
			})
		})

		Context("when SQL injection is attempted via set_config", func() {
			It("treats malicious input as a literal string", func() {
				setupTenant("iso-sqli", "SQLI Corp")
				conn := appUserConn()

				execAsTenant(conn, "iso-sqli", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'setup') ON CONFLICT DO NOTHING",
						"job-sqli-1", "iso-sqli")
					Expect(err).NotTo(HaveOccurred())
				})

				malicious := "' OR '1'='1"
				execAsTenant(conn, malicious, func(tx *sql.Tx) {
					var count int
					Expect(tx.QueryRowContext(context.Background(), "SELECT count(*) FROM jobs").Scan(&count)).To(Succeed())
					Expect(count).To(Equal(0))
				})

				// Verify the jobs table is still intact.
				execAsTenant(conn, "iso-sqli", func(tx *sql.Tx) {
					var count int
					Expect(tx.QueryRowContext(context.Background(), "SELECT count(*) FROM jobs").Scan(&count)).To(Succeed())
					Expect(count).To(BeNumerically(">=", 1))
				})
			})
		})

		Context("when attempting tenant reassignment via UPDATE", func() {
			It("blocks UPDATE of tenant_id via RLS WITH CHECK", func() {
				setupTenant("iso-reassign-src", "Reassign Src")
				setupTenant("iso-reassign-dst", "Reassign Dst")
				conn := appUserConn()

				execAsTenant(conn, "iso-reassign-src", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'setup') ON CONFLICT DO NOTHING",
						"job-reassign-1", "iso-reassign-src")
					Expect(err).NotTo(HaveOccurred())
				})

				ctx := context.Background()
				tx, err := conn.BeginTx(ctx, nil)
				Expect(err).NotTo(HaveOccurred())
				defer tx.Rollback() //nolint:errcheck

				_, err = tx.ExecContext(ctx, "SELECT set_config('app.current_tenant', $1, true)", "iso-reassign-src")
				Expect(err).NotTo(HaveOccurred())
				_, err = tx.ExecContext(ctx,
					"UPDATE jobs SET tenant_id = $1 WHERE job_id = $2",
					"iso-reassign-dst", "job-reassign-1")
				Expect(err).To(HaveOccurred(), "UPDATE tenant_id should be blocked by RLS WITH CHECK")

				// Verify the job is still owned by the source tenant.
				execAsTenant(conn, "iso-reassign-src", func(tx *sql.Tx) {
					var count int
					Expect(tx.QueryRowContext(context.Background(),
						"SELECT count(*) FROM jobs WHERE job_id = $1", "job-reassign-1").Scan(&count)).To(Succeed())
					Expect(count).To(Equal(1))
				})
			})
		})

		Context("when a transaction is rolled back", func() {
			It("clears SET LOCAL tenant context", func() {
				setupTenant("iso-rollback", "Rollback Corp")
				conn := appUserConn()
				ctx := context.Background()

				tx, err := conn.BeginTx(ctx, nil)
				Expect(err).NotTo(HaveOccurred())

				_, err = tx.ExecContext(ctx, "SELECT set_config('app.current_tenant', $1, true)", "iso-rollback")
				Expect(err).NotTo(HaveOccurred())

				var tenantInTx string
				Expect(tx.QueryRowContext(ctx, "SELECT current_setting('app.current_tenant', true)").Scan(&tenantInTx)).To(Succeed())
				Expect(tenantInTx).To(Equal("iso-rollback"))

				Expect(tx.Rollback()).To(Succeed())

				// After rollback, SET LOCAL should have been reverted.
				tx2, err := conn.BeginTx(ctx, nil)
				Expect(err).NotTo(HaveOccurred())
				defer tx2.Rollback() //nolint:errcheck

				var tenantAfter string
				Expect(tx2.QueryRowContext(ctx, "SELECT current_setting('app.current_tenant', true)").Scan(&tenantAfter)).To(Succeed())
				Expect(tenantAfter).NotTo(Equal("iso-rollback"), "SET LOCAL tenant persisted after rollback")
			})
		})
	})

	// ===================================================================
	// Job Ownership
	// ===================================================================

	Describe("Job Ownership", func() {
		It("isolates jobs between users within the same tenant", func() {
			setupTenant("own-tenant", "Ownership Corp")
			conn := appUserConn()

			execAsTenantUser(conn, "own-tenant", "alice", func(tx *sql.Tx) {
				_, err := tx.ExecContext(context.Background(),
					"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', $3) ON CONFLICT DO NOTHING",
					"job-own-alice-1", "own-tenant", "alice")
				Expect(err).NotTo(HaveOccurred())
			})

			execAsTenantUser(conn, "own-tenant", "bob", func(tx *sql.Tx) {
				_, err := tx.ExecContext(context.Background(),
					"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', $3) ON CONFLICT DO NOTHING",
					"job-own-bob-1", "own-tenant", "bob")
				Expect(err).NotTo(HaveOccurred())
			})

			// Alice sees only her own job.
			execAsTenantUser(conn, "own-tenant", "alice", func(tx *sql.Tx) {
				rows, err := tx.QueryContext(context.Background(), "SELECT job_id, created_by FROM jobs")
				Expect(err).NotTo(HaveOccurred())
				defer rows.Close()

				var ids []string
				for rows.Next() {
					var id, creator string
					Expect(rows.Scan(&id, &creator)).To(Succeed())
					Expect(creator).To(Equal("alice"), fmt.Sprintf("alice sees job by %q (id=%s)", creator, id))
					ids = append(ids, id)
				}
				Expect(ids).NotTo(BeEmpty(), "alice sees zero jobs")
			})

			// Service-level (tenant only, no user) sees both.
			execAsTenant(conn, "own-tenant", func(tx *sql.Tx) {
				var count int
				Expect(tx.QueryRowContext(context.Background(), "SELECT count(*) FROM jobs").Scan(&count)).To(Succeed())
				Expect(count).To(BeNumerically(">=", 2))
			})
		})
	})

	// ===================================================================
	// Immutability Triggers
	// ===================================================================

	Describe("Immutability Triggers", func() {
		Context("completed jobs", func() {
			It("blocks UPDATE of a completed job", func() {
				setupTenant("imm-update", "Imm Update Corp")
				conn := appUserConn()

				execAsTenant(conn, "imm-update", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'completed', 'setup') ON CONFLICT DO NOTHING",
						"job-imm-upd-1", "imm-update")
					Expect(err).NotTo(HaveOccurred())
				})

				expectErrorAsTenant(conn, "imm-update", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"UPDATE jobs SET config = '{\"retry\": true}' WHERE job_id = $1", "job-imm-upd-1")
					Expect(err).To(HaveOccurred(), "update of completed job should be blocked")
					Expect(err.Error()).To(ContainSubstring("completed"))
				})
			})

			It("blocks DELETE of a completed job", func() {
				setupTenant("imm-del", "Imm Delete Corp")
				conn := appUserConn()

				execAsTenant(conn, "imm-del", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'completed', 'setup') ON CONFLICT DO NOTHING",
						"job-imm-del-1", "imm-del")
					Expect(err).NotTo(HaveOccurred())
				})

				expectErrorAsTenant(conn, "imm-del", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"DELETE FROM jobs WHERE job_id = $1", "job-imm-del-1")
					Expect(err).To(HaveOccurred(), "delete of completed job should be blocked")
					Expect(err.Error()).To(ContainSubstring("completed"))
				})
			})
		})

		Context("pending jobs", func() {
			It("allows UPDATE of a pending job", func() {
				setupTenant("imm-pend", "Imm Pending Corp")
				conn := appUserConn()

				execAsTenant(conn, "imm-pend", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'setup') ON CONFLICT DO NOTHING",
						"job-imm-pend-1", "imm-pend")
					Expect(err).NotTo(HaveOccurred())
				})

				execAsTenant(conn, "imm-pend", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"UPDATE jobs SET config = '{\"updated\": true}' WHERE job_id = $1", "job-imm-pend-1")
					Expect(err).NotTo(HaveOccurred())
				})
			})

			It("allows transition from pending to completed", func() {
				setupTenant("imm-trans", "Imm Transition Corp")
				jobID := testID("job-imm-trans")
				DeferCleanup(func() { cleanupJobs(jobID) })
				conn := appUserConn()

				execAsTenant(conn, "imm-trans", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'setup')",
						jobID, "imm-trans")
					Expect(err).NotTo(HaveOccurred())
				})

				execAsTenant(conn, "imm-trans", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"UPDATE jobs SET status = 'completed' WHERE job_id = $1", jobID)
					Expect(err).NotTo(HaveOccurred())
				})
			})
		})

		Context("classification write-once", func() {
			It("blocks UPDATE and DELETE on classifications", func() {
				setupTenant("imm-class", "Imm Class Corp")
				conn := appUserConn()

				// Insert a catalog first (classifications have an FK to catalogs).
				execAsTenant(conn, "imm-class", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path) VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING",
						"cat-imm-class-1", "imm-class", "test-catalog", "1.0", "oscal", "/path/to/catalog")
					Expect(err).NotTo(HaveOccurred())
				})

				execAsTenant(conn, "imm-class", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"INSERT INTO classifications (catalog_id, control_id, type, level, tenant_id) VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING",
						"cat-imm-class-1", "AC-1", "nist", "moderate", "imm-class")
					Expect(err).NotTo(HaveOccurred())
				})

				// Update classification is blocked.
				expectErrorAsTenant(conn, "imm-class", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"UPDATE classifications SET level = 'high' WHERE catalog_id = $1 AND control_id = $2 AND type = $3",
						"cat-imm-class-1", "AC-1", "nist")
					Expect(err).To(HaveOccurred(), "update classification should be blocked (write-once)")
					Expect(err.Error()).To(ContainSubstring("classification"))
				})

				// Delete classification is blocked.
				expectErrorAsTenant(conn, "imm-class", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"DELETE FROM classifications WHERE catalog_id = $1 AND control_id = $2 AND type = $3",
						"cat-imm-class-1", "AC-1", "nist")
					Expect(err).To(HaveOccurred(), "delete classification should be blocked (write-once)")
					Expect(err.Error()).To(ContainSubstring("classification"))
				})
			})
		})

		Context("vote summaries for completed jobs", func() {
			It("blocks UPDATE and DELETE when parent job is completed", func() {
				setupTenant("imm-vote-c", "Imm VoteC Corp")
				conn := appUserConn()

				execAsTenant(conn, "imm-vote-c", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'completed', 'setup') ON CONFLICT DO NOTHING",
						"job-imm-vote-c-1", "imm-vote-c")
					Expect(err).NotTo(HaveOccurred())
					_, err = tx.ExecContext(context.Background(),
						"INSERT INTO vote_summaries (job_id, source_id, target_id, consensus, confidence, viability, tenant_id) VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING",
						"job-imm-vote-c-1", "src-1", "tgt-1", "agree", 0.95, 0.90, "imm-vote-c")
					Expect(err).NotTo(HaveOccurred())
				})

				expectErrorAsTenant(conn, "imm-vote-c", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"UPDATE vote_summaries SET confidence = 0.5 WHERE job_id = $1 AND source_id = $2 AND target_id = $3",
						"job-imm-vote-c-1", "src-1", "tgt-1")
					Expect(err).To(HaveOccurred(), "update vote_summary of completed job should be blocked")
					Expect(err.Error()).To(ContainSubstring("completed"))
				})

				expectErrorAsTenant(conn, "imm-vote-c", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"DELETE FROM vote_summaries WHERE job_id = $1 AND source_id = $2 AND target_id = $3",
						"job-imm-vote-c-1", "src-1", "tgt-1")
					Expect(err).To(HaveOccurred(), "delete vote_summary of completed job should be blocked")
					Expect(err.Error()).To(ContainSubstring("completed"))
				})
			})
		})

		Context("vote summaries for pending jobs", func() {
			It("allows UPDATE when parent job is pending", func() {
				setupTenant("imm-vote-p", "Imm VoteP Corp")
				conn := appUserConn()

				execAsTenant(conn, "imm-vote-p", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'setup') ON CONFLICT DO NOTHING",
						"job-imm-vote-p-1", "imm-vote-p")
					Expect(err).NotTo(HaveOccurred())
					_, err = tx.ExecContext(context.Background(),
						"INSERT INTO vote_summaries (job_id, source_id, target_id, consensus, confidence, viability, tenant_id) VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING",
						"job-imm-vote-p-1", "src-1", "tgt-1", "agree", 0.9, 0.8, "imm-vote-p")
					Expect(err).NotTo(HaveOccurred())
				})

				execAsTenant(conn, "imm-vote-p", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"UPDATE vote_summaries SET confidence = 0.75 WHERE job_id = $1 AND source_id = $2 AND target_id = $3",
						"job-imm-vote-p-1", "src-1", "tgt-1")
					Expect(err).NotTo(HaveOccurred())
				})
			})
		})

		Context("job stages for completed jobs", func() {
			It("blocks UPDATE and DELETE when parent job is completed", func() {
				setupTenant("imm-stage", "Imm Stage Corp")
				conn := appUserConn()

				execAsTenant(conn, "imm-stage", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'completed', 'setup') ON CONFLICT DO NOTHING",
						"job-imm-stage-1", "imm-stage")
					Expect(err).NotTo(HaveOccurred())
					_, err = tx.ExecContext(context.Background(),
						"INSERT INTO job_stages (job_id, stage_name, status, tenant_id) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING",
						"job-imm-stage-1", "parse", "completed", "imm-stage")
					Expect(err).NotTo(HaveOccurred())
				})

				expectErrorAsTenant(conn, "imm-stage", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"UPDATE job_stages SET status = 'failed' WHERE job_id = $1 AND stage_name = $2",
						"job-imm-stage-1", "parse")
					Expect(err).To(HaveOccurred(), "update job_stage of completed job should be blocked")
					Expect(err.Error()).To(ContainSubstring("completed"))
				})

				expectErrorAsTenant(conn, "imm-stage", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"DELETE FROM job_stages WHERE job_id = $1 AND stage_name = $2",
						"job-imm-stage-1", "parse")
					Expect(err).To(HaveOccurred(), "delete job_stage of completed job should be blocked")
					Expect(err.Error()).To(ContainSubstring("completed"))
				})
			})
		})

		Context("bulk operations with mixed status", func() {
			It("fails entirely when any row is completed (no partial success)", func() {
				setupTenant("imm-bulk", "Imm Bulk Corp")
				conn := appUserConn()

				execAsTenant(conn, "imm-bulk", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'setup') ON CONFLICT DO NOTHING",
						"job-imm-bulk-pend", "imm-bulk")
					Expect(err).NotTo(HaveOccurred())
					_, err = tx.ExecContext(context.Background(),
						"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'completed', 'setup') ON CONFLICT DO NOTHING",
						"job-imm-bulk-done", "imm-bulk")
					Expect(err).NotTo(HaveOccurred())
				})

				expectErrorAsTenant(conn, "imm-bulk", func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"UPDATE jobs SET config = '{\"bulk\": true}'")
					Expect(err).To(HaveOccurred(), "bulk update touching completed job should fail entirely")
					Expect(err.Error()).To(ContainSubstring("completed"))
				})

				// Verify no partial success.
				execAsTenant(conn, "imm-bulk", func(tx *sql.Tx) {
					var config sql.NullString
					err := tx.QueryRowContext(context.Background(),
						"SELECT config FROM jobs WHERE job_id = $1", "job-imm-bulk-pend").Scan(&config)
					Expect(err).NotTo(HaveOccurred())
					if config.Valid {
						Expect(config.String).NotTo(ContainSubstring("bulk"),
							"partial update leaked: pending job was modified despite completed job blocking")
					}
				})
			})
		})
	})

	// ===================================================================
	// Threat Model - Tired Admin
	// ===================================================================

	Describe("Threat Model - Tired Admin", func() {
		It("blocks resetting a completed job to pending", func() {
			setupTenant("tired-reset", "Tired Reset Corp")
			conn := appUserConn()

			execAsTenant(conn, "tired-reset", func(tx *sql.Tx) {
				_, err := tx.ExecContext(context.Background(),
					"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'completed', 'setup') ON CONFLICT DO NOTHING",
					"job-tired-reset-1", "tired-reset")
				Expect(err).NotTo(HaveOccurred())
			})

			expectErrorAsTenant(conn, "tired-reset", func(tx *sql.Tx) {
				_, err := tx.ExecContext(context.Background(),
					"UPDATE jobs SET status = 'pending' WHERE job_id = $1", "job-tired-reset-1")
				Expect(err).To(HaveOccurred(), "resetting completed job to pending should be blocked")
				Expect(err.Error()).To(ContainSubstring("completed"))
			})

			// Verify status unchanged.
			execAsTenant(conn, "tired-reset", func(tx *sql.Tx) {
				var status string
				Expect(tx.QueryRowContext(context.Background(),
					"SELECT status FROM jobs WHERE job_id = $1", "job-tired-reset-1").Scan(&status)).To(Succeed())
				Expect(status).To(Equal("completed"))
			})
		})

		It("fails bulk config update when any job is completed", func() {
			setupTenant("tired-bulk", "Tired Bulk Corp")
			conn := appUserConn()

			execAsTenant(conn, "tired-bulk", func(tx *sql.Tx) {
				_, err := tx.ExecContext(context.Background(),
					"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'setup') ON CONFLICT DO NOTHING",
					"job-tired-bulk-1", "tired-bulk")
				Expect(err).NotTo(HaveOccurred())
				_, err = tx.ExecContext(context.Background(),
					"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'completed', 'setup') ON CONFLICT DO NOTHING",
					"job-tired-bulk-2", "tired-bulk")
				Expect(err).NotTo(HaveOccurred())
			})

			expectErrorAsTenant(conn, "tired-bulk", func(tx *sql.Tx) {
				_, err := tx.ExecContext(context.Background(),
					"UPDATE jobs SET config = '{\"global\": true}'")
				Expect(err).To(HaveOccurred(), "bulk config update should fail when any job is completed")
			})
		})

		It("blocks DELETE-and-reinsert of a completed job", func() {
			setupTenant("tired-reinst", "Tired Reinsert Corp")
			conn := appUserConn()

			execAsTenant(conn, "tired-reinst", func(tx *sql.Tx) {
				_, err := tx.ExecContext(context.Background(),
					"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'completed', 'setup') ON CONFLICT DO NOTHING",
					"job-tired-reinst-1", "tired-reinst")
				Expect(err).NotTo(HaveOccurred())
			})

			expectErrorAsTenant(conn, "tired-reinst", func(tx *sql.Tx) {
				_, err := tx.ExecContext(context.Background(),
					"DELETE FROM jobs WHERE job_id = $1", "job-tired-reinst-1")
				Expect(err).To(HaveOccurred(), "delete of completed job should be blocked")
				Expect(err.Error()).To(ContainSubstring("completed"))
			})
		})

		It("denies TRUNCATE permission to app_user", func() {
			conn := appUserConn()
			_, err := conn.ExecContext(context.Background(), "TRUNCATE TABLE jobs CASCADE")
			Expect(err).To(HaveOccurred(), "app_user should not have TRUNCATE permission")
			Expect(err.Error()).To(ContainSubstring("permission denied"))
		})

		It("denies ALTER TABLE permission to app_user", func() {
			conn := appUserConn()
			_, err := conn.ExecContext(context.Background(), "ALTER TABLE jobs ADD COLUMN hacked TEXT")
			Expect(err).To(HaveOccurred(), "app_user should not have ALTER TABLE permission")
			Expect(err.Error()).To(SatisfyAny(
				ContainSubstring("permission denied"),
				ContainSubstring("must be owner"),
			))
		})

		It("denies DISABLE RLS to app_user", func() {
			conn := appUserConn()
			_, err := conn.ExecContext(context.Background(), "ALTER TABLE jobs DISABLE ROW LEVEL SECURITY")
			Expect(err).To(HaveOccurred(), "app_user should not be able to disable RLS")
			Expect(err.Error()).To(SatisfyAny(
				ContainSubstring("permission denied"),
				ContainSubstring("must be owner"),
			))
		})

		It("denies DISABLE TRIGGER to app_user", func() {
			conn := appUserConn()
			_, err := conn.ExecContext(context.Background(), "ALTER TABLE jobs DISABLE TRIGGER jobs_immutable_update")
			Expect(err).To(HaveOccurred(), "app_user should not be able to disable triggers")
			Expect(err.Error()).To(SatisfyAny(
				ContainSubstring("permission denied"),
				ContainSubstring("must be owner"),
			))
		})

		It("denies COPY TO to app_user", func() {
			conn := appUserConn()
			_, err := conn.ExecContext(context.Background(), "COPY jobs TO '/tmp/exfil.csv' CSV")
			Expect(err).To(HaveOccurred(), "app_user should not have COPY TO permission")
			Expect(err.Error()).To(SatisfyAny(
				ContainSubstring("permission denied"),
				ContainSubstring("superuser"),
				ContainSubstring("pg_read_server_files"),
			))
		})
	})

	// ===================================================================
	// Graph Lifecycle
	// ===================================================================

	Describe("Graph Lifecycle", func() {
		It("auto-creates a graph when a tenant is inserted", func() {
			tenantID := "graph-create"
			setupTenant(tenantID, "Graph Create Corp")

			ctx := context.Background()
			var graphName string
			row := suPool.QueryRow(ctx,
				"SELECT name FROM ag_catalog.ag_graph WHERE name = $1",
				"crosscodex_"+tenantID)
			Expect(row.Scan(&graphName)).To(Succeed())
			Expect(graphName).To(Equal("crosscodex_" + tenantID))
		})

		It("drops the graph when a tenant is deleted", func() {
			tenantID := "graph-drop"
			setupTenant(tenantID, "Graph Drop Corp")

			ctx := context.Background()

			// Verify graph exists.
			var graphName string
			row := suPool.QueryRow(ctx,
				"SELECT name FROM ag_catalog.ag_graph WHERE name = $1",
				"crosscodex_"+tenantID)
			Expect(row.Scan(&graphName)).To(Succeed())

			// Delete the tenant (superuser, bypasses RLS).
			Expect(suPool.Exec(ctx, "DELETE FROM tenants WHERE tenant_id = $1", tenantID)).To(Succeed())

			// Verify graph was dropped.
			row = suPool.QueryRow(ctx,
				"SELECT name FROM ag_catalog.ag_graph WHERE name = $1",
				"crosscodex_"+tenantID)
			err := row.Scan(&graphName)
			Expect(err).To(HaveOccurred(), "graph still exists after tenant deletion")
		})

		It("returns the correct graph name via tenant_graph_name()", func() {
			conn := appUserConn()
			tenantID := "graph-name-fn"
			setupTenant(tenantID, "Graph Name Corp")

			execAsTenant(conn, tenantID, func(tx *sql.Tx) {
				var name string
				Expect(tx.QueryRowContext(context.Background(),
					"SELECT tenant_graph_name()").Scan(&name)).To(Succeed())
				Expect(name).To(Equal("crosscodex_" + tenantID))
			})
		})

		It("rejects mismatched graph names via assert_tenant_graph()", func() {
			conn := appUserConn()
			tenantID := "graph-assert"
			setupTenant(tenantID, "Graph Assert Corp")

			expectErrorAsTenant(conn, tenantID, func(tx *sql.Tx) {
				_, err := tx.ExecContext(context.Background(),
					"SELECT assert_tenant_graph($1)", "wrong_graph_name")
				Expect(err).To(HaveOccurred(), "assert_tenant_graph with wrong name should fail")
				Expect(err.Error()).To(ContainSubstring("does not match"))
			})
		})
	})

	// ===================================================================
	// TenantPool
	// ===================================================================

	Describe("TenantPool", func() {
		It("sets app.current_tenant via Begin", func() {
			tenantID := "tpool-tenant"
			setupTenant(tenantID, "TPool Tenant Corp")

			pool, err := db.NewPool(db.PoolConfig{
				DSN:          appUserDSN(),
				MaxOpenConns: 2,
			})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(pool.Close)

			tp := db.NewTenantPool(pool)
			ctx, err := tenant.WithTenant(context.Background(), tenantID)
			Expect(err).NotTo(HaveOccurred())

			tx, err := tp.Begin(ctx)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(tx.Rollback)

			row := tx.QueryRow(context.Background(), "SELECT current_setting('app.current_tenant', true)")
			var got string
			Expect(row.Scan(&got)).To(Succeed())
			Expect(got).To(Equal(tenantID))
		})

		It("sets app.current_user via Begin when user is in context", func() {
			tenantID := "tpool-user"
			userID := "test-alice"
			setupTenant(tenantID, "TPool User Corp")

			pool, err := db.NewPool(db.PoolConfig{
				DSN:          appUserDSN(),
				MaxOpenConns: 2,
			})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(pool.Close)

			tp := db.NewTenantPool(pool)
			ctx, err := tenant.WithTenant(context.Background(), tenantID)
			Expect(err).NotTo(HaveOccurred())
			ctx = tenant.WithUser(ctx, userID)

			tx, err := tp.Begin(ctx)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(tx.Rollback)

			row := tx.QueryRow(context.Background(), "SELECT current_setting('app.current_user', true)")
			var got string
			Expect(row.Scan(&got)).To(Succeed())
			Expect(got).To(Equal(userID))
		})

		It("enforces RLS isolation via context propagation", func() {
			tenantA := "ctx-prop-alpha"
			tenantB := "ctx-prop-bravo"
			setupTenant(tenantA, "Context Prop Alpha")
			setupTenant(tenantB, "Context Prop Bravo")

			jobA := testID("ctx-prop-a")
			jobB := testID("ctx-prop-b")
			DeferCleanup(func() { cleanupJobs(jobA, jobB) })

			pool, err := db.NewPool(db.PoolConfig{
				DSN:          appUserDSN(),
				MaxOpenConns: 2,
			})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(pool.Close)

			tp := db.NewTenantPool(pool)

			// Insert a job as tenant A.
			ctxA, err := tenant.WithTenant(context.Background(), tenantA)
			Expect(err).NotTo(HaveOccurred())

			txA, err := tp.Begin(ctxA)
			Expect(err).NotTo(HaveOccurred())
			err = txA.Exec(context.Background(),
				"INSERT INTO jobs (tenant_id, job_id, created_by, status) VALUES ($1, $2, $3, $4)",
				tenantA, jobA, "test-user", "pending")
			Expect(err).NotTo(HaveOccurred())
			Expect(txA.Commit()).To(Succeed())

			// Insert a job as tenant B.
			ctxB, err := tenant.WithTenant(context.Background(), tenantB)
			Expect(err).NotTo(HaveOccurred())

			txB, err := tp.Begin(ctxB)
			Expect(err).NotTo(HaveOccurred())
			err = txB.Exec(context.Background(),
				"INSERT INTO jobs (tenant_id, job_id, created_by, status) VALUES ($1, $2, $3, $4)",
				tenantB, jobB, "test-user", "pending")
			Expect(err).NotTo(HaveOccurred())
			Expect(txB.Commit()).To(Succeed())

			// Query as tenant A -- should see only A's job.
			txA2, err := tp.Begin(ctxA)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(txA2.Rollback)

			rows, err := txA2.Query(context.Background(),
				"SELECT job_id, tenant_id FROM jobs WHERE job_id IN ($1, $2)",
				jobA, jobB)
			Expect(err).NotTo(HaveOccurred())
			defer rows.Close()

			var found []string
			for rows.Next() {
				var id, tid string
				Expect(rows.Scan(&id, &tid)).To(Succeed())
				Expect(tid).To(Equal(tenantA), "RLS leaked: got tenant_id=%q in tenant A's query")
				found = append(found, id)
			}
			Expect(rows.Err()).NotTo(HaveOccurred())
			Expect(found).To(ConsistOf(jobA))
		})

		It("rejects invalid tenant IDs", func() {
			pool, err := db.NewPool(db.PoolConfig{
				DSN:          appUserDSN(),
				MaxOpenConns: 2,
			})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(pool.Close)

			tp := db.NewTenantPool(pool)

			// WithTenant should reject invalid IDs.
			_, wtErr := tenant.WithTenant(context.Background(), "BAD!")
			Expect(wtErr).To(HaveOccurred())

			// A bare context should be rejected by TenantPool.Begin.
			_, beginErr := tp.Begin(context.Background())
			Expect(errors.Is(beginErr, db.ErrTenantRequired)).To(BeTrue())
		})
	})

	// ===================================================================
	// Role Isolation
	// ===================================================================

	Describe("Role Isolation", func() {
		// --- Positive: app_user can do its job ---
		Context("app_user positive capabilities", func() {
			It("can query relational data with tenant context", func() {
				tenantID := "role-appuser-dml"
				setupTenant(tenantID, "AppUser DML Corp")
				conn := appUserConn()

				execAsTenant(conn, tenantID, func(tx *sql.Tx) {
					_, err := tx.ExecContext(context.Background(),
						"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'test') ON CONFLICT DO NOTHING",
						"job-role-dml-1", tenantID)
					Expect(err).NotTo(HaveOccurred())

					var count int
					Expect(tx.QueryRowContext(context.Background(),
						"SELECT count(*) FROM jobs WHERE job_id = $1", "job-role-dml-1").Scan(&count)).To(Succeed())
					Expect(count).To(Equal(1))
				})
			})
		})

		// --- Positive: graph_user can do its job ---
		Context("graph_user positive capabilities", func() {
			It("can query ag_catalog.ag_graph", func() {
				tenantID := "role-graphcat"
				setupTenant(tenantID, "Graph Catalog Corp")
				conn := graphUserConn()

				var count int
				err := conn.QueryRowContext(context.Background(),
					"SELECT count(*) FROM ag_catalog.ag_graph WHERE name = $1",
					"crosscodex_"+tenantID).Scan(&count)
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(1))
			})

			It("can access per-tenant graph schema tables", func() {
				tenantID := "role-graphschema"
				setupTenant(tenantID, "Graph Schema Corp")
				conn := graphUserConn()

				schemaName := "crosscodex_" + tenantID
				query := fmt.Sprintf(
					`SELECT count(*) FROM "%s"._ag_label_vertex`, schemaName)
				var count int
				Expect(conn.QueryRowContext(context.Background(), query).Scan(&count)).To(Succeed())
			})
		})

		// --- Negative: app_user blocked from graph layer ---
		Context("app_user blocked from graph layer", func() {
			It("cannot read ag_catalog", func() {
				conn := appUserConn()
				_, err := conn.ExecContext(context.Background(), "SELECT count(*) FROM ag_catalog.ag_graph")
				Expect(err).To(HaveOccurred(), "app_user should not be able to read ag_catalog.ag_graph")
				Expect(err.Error()).To(ContainSubstring("permission denied"))
			})

			It("cannot read per-tenant graph schema tables", func() {
				tenantID := "role-appgraph"
				setupTenant(tenantID, "AppGraph Corp")
				conn := appUserConn()

				schemaName := "crosscodex_" + tenantID
				query := fmt.Sprintf(
					`SELECT count(*) FROM "%s"._ag_label_vertex`, schemaName)
				_, err := conn.ExecContext(context.Background(), query)
				Expect(err).To(HaveOccurred(), "app_user should not be able to read graph schema")
				Expect(err.Error()).To(ContainSubstring("permission denied"))
			})

			It("cannot call ag_catalog.cypher()", func() {
				tenantID := "role-appnocypher"
				setupTenant(tenantID, "AppNoCypher Corp")
				conn := appUserConn()

				query := fmt.Sprintf(
					"SELECT * FROM ag_catalog.cypher('crosscodex_%s', $$ MATCH (n) RETURN n $$) AS (v agtype)",
					tenantID)
				_, err := conn.ExecContext(context.Background(), query)
				Expect(err).To(HaveOccurred(), "app_user should not be able to call ag_catalog.cypher()")
				Expect(err.Error()).To(ContainSubstring("permission denied"))
			})

			It("cannot call ag_catalog.create_graph()", func() {
				conn := appUserConn()
				_, err := conn.ExecContext(context.Background(), "SELECT ag_catalog.create_graph('rogue_graph')")
				Expect(err).To(HaveOccurred(), "app_user should not be able to call ag_catalog.create_graph()")
				Expect(err.Error()).To(ContainSubstring("permission denied"))
			})
		})

		// --- Negative: graph_user blocked from relational layer ---
		Context("graph_user blocked from relational layer", func() {
			It("cannot read tenants table", func() {
				conn := graphUserConn()
				_, err := conn.ExecContext(context.Background(), "SELECT count(*) FROM tenants")
				Expect(err).To(HaveOccurred(), "graph_user should not be able to read tenants table")
				Expect(err.Error()).To(ContainSubstring("permission denied"))
			})

			It("cannot read jobs table", func() {
				conn := graphUserConn()
				_, err := conn.ExecContext(context.Background(), "SELECT count(*) FROM jobs")
				Expect(err).To(HaveOccurred(), "graph_user should not be able to read jobs table")
				Expect(err.Error()).To(ContainSubstring("permission denied"))
			})

			It("cannot insert into tenants table", func() {
				conn := graphUserConn()
				_, err := conn.ExecContext(context.Background(),
					"INSERT INTO tenants (tenant_id, display_name) VALUES ('rogue-tenant', 'Rogue Corp')")
				Expect(err).To(HaveOccurred(), "graph_user should not be able to insert into tenants")
				Expect(err.Error()).To(ContainSubstring("permission denied"))
			})

			It("cannot truncate relational tables", func() {
				conn := graphUserConn()
				_, err := conn.ExecContext(context.Background(), "TRUNCATE TABLE jobs CASCADE")
				Expect(err).To(HaveOccurred(), "graph_user should not be able to truncate jobs")
				Expect(err.Error()).To(ContainSubstring("permission denied"))
			})

			It("cannot alter relational tables", func() {
				conn := graphUserConn()
				_, err := conn.ExecContext(context.Background(), "ALTER TABLE jobs ADD COLUMN hacked TEXT")
				Expect(err).To(HaveOccurred(), "graph_user should not be able to alter relational tables")
				Expect(err.Error()).To(SatisfyAny(
					ContainSubstring("permission denied"),
					ContainSubstring("must be owner"),
				))
			})
		})
	})

	// ===================================================================
	// Telemetry
	// ===================================================================

	Describe("Telemetry", func() {
		It("emits spans and metrics for pool operations", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { tp.Shutdown(context.Background()) }) //nolint:errcheck

			tracer := tp.TracerProvider().Tracer("crosscodex/pkg/db/test")
			meter := tp.MeterProvider().Meter("crosscodex/pkg/db/test")

			pool, err := db.NewPool(db.PoolConfig{
				DSN:          appUserDSN(),
				MaxOpenConns: 2,
			}, db.WithTelemetry(tracer, meter))
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(pool.Close)

			ctx := context.Background()

			// Exercise Query
			rows, err := pool.Query(ctx, "SELECT 1")
			Expect(err).NotTo(HaveOccurred())
			rows.Close()

			// Exercise QueryRow
			row := pool.QueryRow(ctx, "SELECT 1")
			var dummy int
			_ = row.Scan(&dummy)

			// Exercise Exec
			Expect(pool.Exec(ctx, "SELECT 1")).To(Succeed())

			// Exercise Begin + Commit
			tx, err := pool.Begin(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(tx.Commit()).To(Succeed())

			// Exercise Health
			_, err = pool.Health(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Assert spans
			spans := tp.GetSpans()
			for _, name := range []string{"db.Query", "db.QueryRow", "db.Exec", "db.Begin", "db.Health"} {
				Expect(telemetrytest.FindSpan(spans, name)).NotTo(BeNil(), "expected span %q not found", name)
			}

			// Assert metrics
			rm := tp.GetMetrics()

			qm := telemetrytest.FindMetric(rm, "db.queries.total")
			Expect(qm).NotTo(BeNil(), "metric db.queries.total not found")
			qv, err := telemetrytest.CounterValue(qm)
			Expect(err).NotTo(HaveOccurred())
			Expect(qv).To(BeNumerically(">=", int64(3)))

			tm := telemetrytest.FindMetric(rm, "db.transactions.total")
			Expect(tm).NotTo(BeNil(), "metric db.transactions.total not found")
			tv, err := telemetrytest.CounterValue(tm)
			Expect(err).NotTo(HaveOccurred())
			Expect(tv).To(BeNumerically(">=", int64(1)))

			lm := telemetrytest.FindMetric(rm, "db.query.duration_ms")
			Expect(lm).NotTo(BeNil(), "metric db.query.duration_ms not found")
			lc, err := telemetrytest.HistogramCount(lm)
			Expect(err).NotTo(HaveOccurred())
			Expect(lc).To(BeNumerically(">=", int64(2)))

			gm := telemetrytest.FindMetric(rm, "db.pool.open_connections")
			Expect(gm).NotTo(BeNil(), "metric db.pool.open_connections not found")
			gv, err := telemetrytest.GaugeValue(gm)
			Expect(err).NotTo(HaveOccurred())
			Expect(gv).To(BeNumerically(">=", int64(0)))
		})

		It("emits db.TenantBegin span with tenant.id attribute", func() {
			setupTenant("telemetry-tenant", "Telemetry Tenant")

			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { tp.Shutdown(context.Background()) }) //nolint:errcheck

			// WARNING: mutates global TracerProvider. Must NOT run in parallel.
			prev := otel.GetTracerProvider()
			otel.SetTracerProvider(tp.TracerProvider())
			DeferCleanup(func() { otel.SetTracerProvider(prev) })

			pool, err := db.NewPool(db.PoolConfig{
				DSN:          appUserDSN(),
				MaxOpenConns: 2,
			})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(pool.Close)

			tenantPool := db.NewTenantPool(pool)

			tenantCtx, err := tenant.WithTenant(context.Background(), "telemetry-tenant")
			Expect(err).NotTo(HaveOccurred())

			tx, err := tenantPool.Begin(tenantCtx)
			Expect(err).NotTo(HaveOccurred())
			Expect(tx.Commit()).To(Succeed())

			spans := tp.GetSpans()
			s := telemetrytest.FindSpan(spans, "db.TenantBegin")
			Expect(s).NotTo(BeNil(), "expected span db.TenantBegin not found")

			val, ok := telemetrytest.SpanAttribute(s, "tenant.id")
			Expect(ok).To(BeTrue(), "span db.TenantBegin missing tenant.id attribute")
			Expect(val.AsString()).To(Equal("telemetry-tenant"))
		})
	})
})
