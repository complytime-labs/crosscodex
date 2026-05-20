//go:build integration

package db_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/complytime-labs/crosscodex/pkg/db"
)

// suDSN is the superuser connection string, set by TestMain.
var suDSN string

// suPool is the superuser pool for test setup operations.
var suPool db.Pool

func TestMain(m *testing.M) {
	ctx := context.Background()

	suDSN = os.Getenv("TEST_DATABASE_DSN")
	if suDSN == "" {
		fmt.Fprintln(os.Stderr, "TEST_DATABASE_DSN not set — run: task test:integration")
		os.Exit(1)
	}

	// Run migrations as superuser.
	migrator, err := db.NewMigrator(suDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create migrator: %v\n", err)
		os.Exit(1)
	}
	if err := migrator.Up(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "failed to run migrations: %v\n", err)
		os.Exit(1)
	}
	if err := migrator.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to close migrator: %v\n", err)
		os.Exit(1)
	}

	// Set passwords for application roles (migrations create them without one).
	//
	// Two non-superuser roles enforce privilege separation:
	//   app_user   — relational DML + RLS, zero graph access
	//   graph_user — graph schema owner, zero relational access
	// See doc.go "Security Model — Three Roles" for the full design.
	adminDB, err := sql.Open("pgx", suDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open admin connection: %v\n", err)
		os.Exit(1)
	}
	if _, err := adminDB.ExecContext(ctx, "ALTER ROLE app_user WITH PASSWORD 'apppass'"); err != nil {
		fmt.Fprintf(os.Stderr, "failed to set app_user password: %v\n", err)
		os.Exit(1)
	}
	if _, err := adminDB.ExecContext(ctx, "ALTER ROLE graph_user WITH PASSWORD 'graphpass'"); err != nil {
		fmt.Fprintf(os.Stderr, "failed to set graph_user password: %v\n", err)
		os.Exit(1)
	}
	if err := adminDB.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to close admin connection: %v\n", err)
		os.Exit(1)
	}

	// Create superuser pool for setup helpers.
	suPool, err = db.NewPool(db.PoolConfig{
		DSN:          suDSN,
		MaxOpenConns: 5,
		Extensions:   []string{"age", "vector"},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create superuser pool: %v\n", err)
		os.Exit(1)
	}
	defer suPool.Close()

	os.Exit(m.Run())
}

// appUserDSN returns the DSN for app_user, derived from the superuser DSN.
func appUserDSN() string {
	u, err := url.Parse(suDSN)
	if err != nil {
		panic(fmt.Sprintf("bad suDSN: %v", err))
	}
	u.User = url.UserPassword("app_user", "apppass")
	return u.String()
}

// appUserConn opens a raw *sql.DB as app_user. The caller must close it.
func appUserConn(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("pgx", appUserDSN())
	if err != nil {
		t.Fatalf("failed to open app_user connection: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// graphUserDSN returns the DSN for graph_user, derived from the superuser DSN.
func graphUserDSN() string {
	u, err := url.Parse(suDSN)
	if err != nil {
		panic(fmt.Sprintf("bad suDSN: %v", err))
	}
	u.User = url.UserPassword("graph_user", "graphpass")
	return u.String()
}

// graphUserConn opens a raw *sql.DB as graph_user. The caller must close it.
func graphUserConn(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("pgx", graphUserDSN())
	if err != nil {
		t.Fatalf("failed to open graph_user connection: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// setupTenant inserts a tenant via the superuser pool.
// Uses ON CONFLICT DO NOTHING so tests are idempotent.
func setupTenant(t *testing.T, tenantID, displayName string) {
	t.Helper()
	ctx := context.Background()
	err := suPool.Exec(ctx,
		"INSERT INTO tenants (tenant_id, display_name) VALUES ($1, $2) ON CONFLICT DO NOTHING",
		tenantID, displayName)
	if err != nil {
		t.Fatalf("setupTenant(%q): %v", tenantID, err)
	}
}

// execAsTenant runs fn inside a transaction with app.current_tenant set.
func execAsTenant(t *testing.T, conn *sql.DB, tenantID string, fn func(tx *sql.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.current_tenant', $1, true)", tenantID); err != nil {
		t.Fatalf("set_config tenant: %v", err)
	}
	fn(tx)
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// execAsTenantUser runs fn inside a transaction with both tenant and user set.
func execAsTenantUser(t *testing.T, conn *sql.DB, tenantID, userID string, fn func(tx *sql.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.current_tenant', $1, true)", tenantID); err != nil {
		t.Fatalf("set_config tenant: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.current_user', $1, true)", userID); err != nil {
		t.Fatalf("set_config user: %v", err)
	}
	fn(tx)
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// expectErrorAsTenant runs fn inside a transaction with app.current_tenant
// set, then rolls back. Use this instead of execAsTenant when the operation
// inside fn is expected to fail (trigger, RLS). PostgreSQL aborts the
// transaction on error, so Commit() would fail — this helper always rolls
// back, which is correct for negative tests.
func expectErrorAsTenant(t *testing.T, conn *sql.DB, tenantID string, fn func(tx *sql.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.current_tenant', $1, true)", tenantID); err != nil {
		t.Fatalf("set_config tenant: %v", err)
	}
	fn(tx)
}

// ---------------------------------------------------------------------------
// Pool & Lifecycle
// ---------------------------------------------------------------------------

func TestIntegration_PoolConnect(t *testing.T) {
	pool, err := db.NewPool(db.PoolConfig{
		DSN:          suDSN,
		MaxOpenConns: 2,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	ctx := context.Background()
	row := pool.QueryRow(ctx, "SELECT 1")
	var got int
	if err := row.Scan(&got); err != nil {
		t.Fatalf("SELECT 1: %v", err)
	}
	if got != 1 {
		t.Errorf("SELECT 1 = %d, want 1", got)
	}
}

func TestIntegration_PoolHealth(t *testing.T) {
	pool, err := db.NewPool(db.PoolConfig{
		DSN:          suDSN,
		MaxOpenConns: 5,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	ctx := context.Background()
	hs, err := pool.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !hs.Connected {
		t.Error("Health.Connected = false, want true")
	}
	if hs.MaxOpen <= 0 {
		t.Errorf("Health.MaxOpen = %d, want > 0", hs.MaxOpen)
	}
}

func TestIntegration_VerifyExtensions(t *testing.T) {
	pool, err := db.NewPool(db.PoolConfig{
		DSN:          suDSN,
		MaxOpenConns: 2,
		Extensions:   []string{"age", "vector"},
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	ctx := context.Background()
	if err := pool.VerifyExtensions(ctx); err != nil {
		t.Fatalf("VerifyExtensions: %v", err)
	}
}

func TestIntegration_VerifyExtensions_Missing(t *testing.T) {
	pool, err := db.NewPool(db.PoolConfig{
		DSN:          suDSN,
		MaxOpenConns: 2,
		Extensions:   []string{"age", "nonexistent_ext_xyz"},
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	ctx := context.Background()
	err = pool.VerifyExtensions(ctx)
	if err == nil {
		t.Fatal("VerifyExtensions should return error for missing extension")
	}

	var extErr *db.ExtensionError
	if !errors.As(err, &extErr) {
		t.Fatalf("error type = %T, want *db.ExtensionError", err)
	}
	found := false
	for _, m := range extErr.Missing {
		if m == "nonexistent_ext_xyz" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Missing = %v, want to contain nonexistent_ext_xyz", extErr.Missing)
	}
}

func TestIntegration_PoolCloseIdempotent(t *testing.T) {
	pool, err := db.NewPool(db.PoolConfig{
		DSN:          suDSN,
		MaxOpenConns: 2,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	if err := pool.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Second close must not panic.
	if err := pool.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Migrations
// ---------------------------------------------------------------------------

func TestIntegration_MigratorVersion(t *testing.T) {
	migrator, err := db.NewMigrator(suDSN)
	if err != nil {
		t.Fatalf("NewMigrator: %v", err)
	}
	defer migrator.Close()

	ctx := context.Background()
	version, dirty, err := migrator.Version(ctx)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if version != 9 {
		t.Errorf("version = %d, want 9", version)
	}
	if dirty {
		t.Error("dirty = true, want false")
	}
}

func TestIntegration_MigratorUpIdempotent(t *testing.T) {
	migrator, err := db.NewMigrator(suDSN)
	if err != nil {
		t.Fatalf("NewMigrator: %v", err)
	}
	defer migrator.Close()

	ctx := context.Background()
	// Migrations already ran in TestMain. Calling Up again should succeed.
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("Up (idempotent): %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tenant Isolation (RLS)
// ---------------------------------------------------------------------------

func TestIntegration_TenantIsolation_TwoTenants(t *testing.T) {
	setupTenant(t, "iso-alpha", "Alpha Corp")
	setupTenant(t, "iso-beta", "Beta Inc")

	conn := appUserConn(t)

	// Insert a job for alpha.
	execAsTenant(t, conn, "iso-alpha", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'setup') ON CONFLICT DO NOTHING",
			"job-iso-a1", "iso-alpha")
		if err != nil {
			t.Fatalf("insert alpha job: %v", err)
		}
	})

	// Insert a job for beta.
	execAsTenant(t, conn, "iso-beta", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'setup') ON CONFLICT DO NOTHING",
			"job-iso-b1", "iso-beta")
		if err != nil {
			t.Fatalf("insert beta job: %v", err)
		}
	})

	// Query as alpha: should see only alpha's job.
	execAsTenant(t, conn, "iso-alpha", func(tx *sql.Tx) {
		rows, err := tx.QueryContext(context.Background(), "SELECT job_id FROM jobs")
		if err != nil {
			t.Fatalf("query alpha jobs: %v", err)
		}
		defer rows.Close()

		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		for _, id := range ids {
			if !strings.HasPrefix(id, "job-iso-a") {
				t.Errorf("alpha tenant sees non-alpha job: %s", id)
			}
		}
		if len(ids) == 0 {
			t.Error("alpha tenant sees zero jobs, want at least 1")
		}
	})
}

func TestIntegration_TenantIsolation_NoContext(t *testing.T) {
	setupTenant(t, "iso-nocontext", "NoCtx Corp")
	conn := appUserConn(t)

	// Insert a job as the tenant.
	execAsTenant(t, conn, "iso-nocontext", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'setup') ON CONFLICT DO NOTHING",
			"job-nocontext-1", "iso-nocontext")
		if err != nil {
			t.Fatalf("insert job: %v", err)
		}
	})

	// Query without setting tenant: should see zero rows (fail closed).
	ctx := context.Background()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM jobs").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("no-tenant query returned %d rows, want 0", count)
	}
}

func TestIntegration_TenantIsolation_CrossTenantWrite(t *testing.T) {
	setupTenant(t, "iso-cross-src", "Source Corp")
	setupTenant(t, "iso-cross-dst", "Dest Corp")

	conn := appUserConn(t)

	// Try to insert a job for dst tenant while authenticated as src tenant.
	// RLS WITH CHECK should block this.
	expectErrorAsTenant(t, conn, "iso-cross-src", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'attacker')",
			"job-cross-write-1", "iso-cross-dst")
		if err == nil {
			t.Error("cross-tenant write should be blocked by RLS WITH CHECK")
		}
	})

	// Verify no data was written for the destination tenant.
	execAsTenant(t, conn, "iso-cross-dst", func(tx *sql.Tx) {
		var count int
		if err := tx.QueryRowContext(context.Background(),
			"SELECT count(*) FROM jobs WHERE job_id = $1", "job-cross-write-1").Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 0 {
			t.Error("cross-tenant write leaked data: count > 0")
		}
	})
}

func TestIntegration_TenantIsolation_EmptyTenantID(t *testing.T) {
	setupTenant(t, "iso-empty", "Empty Corp")
	conn := appUserConn(t)

	// Insert a job.
	execAsTenant(t, conn, "iso-empty", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'setup') ON CONFLICT DO NOTHING",
			"job-empty-1", "iso-empty")
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	})

	// Set tenant to empty string: should match zero rows.
	execAsTenant(t, conn, "", func(tx *sql.Tx) {
		var count int
		if err := tx.QueryRowContext(context.Background(), "SELECT count(*) FROM jobs").Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 0 {
			t.Errorf("empty tenant ID query returned %d rows, want 0", count)
		}
	})
}

func TestIntegration_TenantIsolation_SQLInjection(t *testing.T) {
	setupTenant(t, "iso-sqli", "SQLI Corp")
	conn := appUserConn(t)

	// Insert a job for the legit tenant.
	execAsTenant(t, conn, "iso-sqli", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'setup') ON CONFLICT DO NOTHING",
			"job-sqli-1", "iso-sqli")
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	})

	// Attempt SQL injection via set_config. The parameterized call treats
	// the value as a literal string, not SQL.
	malicious := "' OR '1'='1"
	execAsTenant(t, conn, malicious, func(tx *sql.Tx) {
		var count int
		if err := tx.QueryRowContext(context.Background(), "SELECT count(*) FROM jobs").Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 0 {
			t.Errorf("SQL injection returned %d rows, want 0", count)
		}
	})

	// Verify the jobs table still exists and is intact.
	execAsTenant(t, conn, "iso-sqli", func(tx *sql.Tx) {
		var count int
		if err := tx.QueryRowContext(context.Background(), "SELECT count(*) FROM jobs").Scan(&count); err != nil {
			t.Fatalf("post-injection count: %v", err)
		}
		if count < 1 {
			t.Error("jobs table damaged after injection attempt")
		}
	})
}

func TestIntegration_TenantIsolation_ReassignTenant(t *testing.T) {
	setupTenant(t, "iso-reassign-src", "Reassign Src")
	setupTenant(t, "iso-reassign-dst", "Reassign Dst")

	conn := appUserConn(t)

	// Insert a job for the source tenant.
	execAsTenant(t, conn, "iso-reassign-src", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'setup') ON CONFLICT DO NOTHING",
			"job-reassign-1", "iso-reassign-src")
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	})

	// Try to UPDATE tenant_id to a different tenant. RLS WITH CHECK should block.
	ctx := context.Background()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.current_tenant', $1, true)", "iso-reassign-src"); err != nil {
		t.Fatalf("set_config: %v", err)
	}
	_, err = tx.ExecContext(ctx,
		"UPDATE jobs SET tenant_id = $1 WHERE job_id = $2",
		"iso-reassign-dst", "job-reassign-1")
	if err == nil {
		t.Error("UPDATE tenant_id should be blocked by RLS WITH CHECK")
	}
	// No commit needed; the error rolled it back logically.

	// Verify the job is still owned by the source tenant.
	execAsTenant(t, conn, "iso-reassign-src", func(tx *sql.Tx) {
		var count int
		if err := tx.QueryRowContext(context.Background(),
			"SELECT count(*) FROM jobs WHERE job_id = $1", "job-reassign-1").Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 1 {
			t.Error("job disappeared after failed reassignment")
		}
	})
}

func TestIntegration_TenantIsolation_RollbackClearsTenant(t *testing.T) {
	setupTenant(t, "iso-rollback", "Rollback Corp")
	conn := appUserConn(t)

	ctx := context.Background()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}

	// Set tenant in transaction.
	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.current_tenant', $1, true)", "iso-rollback"); err != nil {
		t.Fatalf("set_config: %v", err)
	}

	// Verify tenant is set within the transaction.
	var tenantInTx string
	if err := tx.QueryRowContext(ctx, "SELECT current_setting('app.current_tenant', true)").Scan(&tenantInTx); err != nil {
		t.Fatalf("current_setting in tx: %v", err)
	}
	if tenantInTx != "iso-rollback" {
		t.Errorf("tenant in tx = %q, want iso-rollback", tenantInTx)
	}

	// Rollback.
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// After rollback, SET LOCAL should have been reverted.
	// Open a new transaction to check.
	tx2, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx2: %v", err)
	}
	defer tx2.Rollback()

	var tenantAfter string
	if err := tx2.QueryRowContext(ctx, "SELECT current_setting('app.current_tenant', true)").Scan(&tenantAfter); err != nil {
		t.Fatalf("current_setting after rollback: %v", err)
	}
	if tenantAfter == "iso-rollback" {
		t.Error("SET LOCAL tenant persisted after rollback, want it cleared")
	}
}

// ---------------------------------------------------------------------------
// Job Ownership
// ---------------------------------------------------------------------------

func TestIntegration_JobOwnership_TwoUsers(t *testing.T) {
	setupTenant(t, "own-tenant", "Ownership Corp")
	conn := appUserConn(t)

	// Insert jobs for alice and bob in the same tenant.
	execAsTenantUser(t, conn, "own-tenant", "alice", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', $3) ON CONFLICT DO NOTHING",
			"job-own-alice-1", "own-tenant", "alice")
		if err != nil {
			t.Fatalf("insert alice job: %v", err)
		}
	})

	execAsTenantUser(t, conn, "own-tenant", "bob", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', $3) ON CONFLICT DO NOTHING",
			"job-own-bob-1", "own-tenant", "bob")
		if err != nil {
			t.Fatalf("insert bob job: %v", err)
		}
	})

	// Alice sees only her own job.
	execAsTenantUser(t, conn, "own-tenant", "alice", func(tx *sql.Tx) {
		rows, err := tx.QueryContext(context.Background(), "SELECT job_id, created_by FROM jobs")
		if err != nil {
			t.Fatalf("query alice: %v", err)
		}
		defer rows.Close()

		var ids []string
		for rows.Next() {
			var id, creator string
			if err := rows.Scan(&id, &creator); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if creator != "alice" {
				t.Errorf("alice sees job by %q (id=%s)", creator, id)
			}
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			t.Error("alice sees zero jobs")
		}
	})

	// Service-level (tenant only, no user) sees both.
	execAsTenant(t, conn, "own-tenant", func(tx *sql.Tx) {
		var count int
		if err := tx.QueryRowContext(context.Background(), "SELECT count(*) FROM jobs").Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if count < 2 {
			t.Errorf("service-level sees %d jobs, want >= 2", count)
		}
	})
}

// ---------------------------------------------------------------------------
// Immutability Triggers
// ---------------------------------------------------------------------------

func TestIntegration_Immutability_CompletedJobUpdate(t *testing.T) {
	setupTenant(t, "imm-update", "Imm Update Corp")
	conn := appUserConn(t)

	// Insert a completed job.
	execAsTenant(t, conn, "imm-update", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'completed', 'setup') ON CONFLICT DO NOTHING",
			"job-imm-upd-1", "imm-update")
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	})

	// Attempt to update it.
	expectErrorAsTenant(t, conn, "imm-update", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"UPDATE jobs SET config = '{\"retry\": true}' WHERE job_id = $1", "job-imm-upd-1")
		if err == nil {
			t.Error("update of completed job should be blocked")
			return
		}
		if !strings.Contains(err.Error(), "completed") {
			t.Errorf("error not actionable: %v", err)
		}
	})
}

func TestIntegration_Immutability_CompletedJobDelete(t *testing.T) {
	setupTenant(t, "imm-del", "Imm Delete Corp")
	conn := appUserConn(t)

	execAsTenant(t, conn, "imm-del", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'completed', 'setup') ON CONFLICT DO NOTHING",
			"job-imm-del-1", "imm-del")
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	})

	expectErrorAsTenant(t, conn, "imm-del", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"DELETE FROM jobs WHERE job_id = $1", "job-imm-del-1")
		if err == nil {
			t.Error("delete of completed job should be blocked")
			return
		}
		if !strings.Contains(err.Error(), "completed") {
			t.Errorf("error not actionable: %v", err)
		}
	})
}

func TestIntegration_Immutability_PendingJobUpdate(t *testing.T) {
	setupTenant(t, "imm-pend", "Imm Pending Corp")
	conn := appUserConn(t)

	execAsTenant(t, conn, "imm-pend", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'setup') ON CONFLICT DO NOTHING",
			"job-imm-pend-1", "imm-pend")
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	})

	execAsTenant(t, conn, "imm-pend", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"UPDATE jobs SET config = '{\"updated\": true}' WHERE job_id = $1", "job-imm-pend-1")
		if err != nil {
			t.Errorf("update pending job should succeed: %v", err)
		}
	})
}

func TestIntegration_Immutability_TransitionToCompleted(t *testing.T) {
	setupTenant(t, "imm-trans", "Imm Transition Corp")
	conn := appUserConn(t)

	execAsTenant(t, conn, "imm-trans", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'setup') ON CONFLICT DO NOTHING",
			"job-imm-trans-1", "imm-trans")
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	})

	// Transitioning from pending to completed should succeed.
	execAsTenant(t, conn, "imm-trans", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"UPDATE jobs SET status = 'completed' WHERE job_id = $1", "job-imm-trans-1")
		if err != nil {
			t.Errorf("transition to completed should succeed: %v", err)
		}
	})
}

func TestIntegration_Immutability_ClassificationWriteOnce(t *testing.T) {
	setupTenant(t, "imm-class", "Imm Class Corp")
	conn := appUserConn(t)

	// Insert a catalog first (classifications have an FK to catalogs).
	execAsTenant(t, conn, "imm-class", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path) VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING",
			"cat-imm-class-1", "imm-class", "test-catalog", "1.0", "oscal", "/path/to/catalog")
		if err != nil {
			t.Fatalf("insert catalog: %v", err)
		}
	})

	// Insert classification succeeds.
	execAsTenant(t, conn, "imm-class", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"INSERT INTO classifications (catalog_id, control_id, type, level, tenant_id) VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING",
			"cat-imm-class-1", "AC-1", "nist", "moderate", "imm-class")
		if err != nil {
			t.Fatalf("insert classification: %v", err)
		}
	})

	// Update classification is blocked.
	expectErrorAsTenant(t, conn, "imm-class", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"UPDATE classifications SET level = 'high' WHERE catalog_id = $1 AND control_id = $2 AND type = $3",
			"cat-imm-class-1", "AC-1", "nist")
		if err == nil {
			t.Error("update classification should be blocked (write-once)")
			return
		}
		if !strings.Contains(err.Error(), "classification") {
			t.Errorf("error not actionable: %v", err)
		}
	})

	// Delete classification is blocked.
	expectErrorAsTenant(t, conn, "imm-class", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"DELETE FROM classifications WHERE catalog_id = $1 AND control_id = $2 AND type = $3",
			"cat-imm-class-1", "AC-1", "nist")
		if err == nil {
			t.Error("delete classification should be blocked (write-once)")
			return
		}
		if !strings.Contains(err.Error(), "classification") {
			t.Errorf("error not actionable: %v", err)
		}
	})
}

func TestIntegration_Immutability_VoteSummary_CompletedJob(t *testing.T) {
	setupTenant(t, "imm-vote-c", "Imm VoteC Corp")
	conn := appUserConn(t)

	// Insert a completed job and its vote summary.
	execAsTenant(t, conn, "imm-vote-c", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'completed', 'setup') ON CONFLICT DO NOTHING",
			"job-imm-vote-c-1", "imm-vote-c")
		if err != nil {
			t.Fatalf("insert job: %v", err)
		}
		_, err = tx.ExecContext(context.Background(),
			"INSERT INTO vote_summaries (job_id, source_id, target_id, consensus, confidence, viability, tenant_id) VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING",
			"job-imm-vote-c-1", "src-1", "tgt-1", "agree", 0.95, 0.90, "imm-vote-c")
		if err != nil {
			t.Fatalf("insert vote_summary: %v", err)
		}
	})

	// Update should be blocked.
	expectErrorAsTenant(t, conn, "imm-vote-c", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"UPDATE vote_summaries SET confidence = 0.5 WHERE job_id = $1 AND source_id = $2 AND target_id = $3",
			"job-imm-vote-c-1", "src-1", "tgt-1")
		if err == nil {
			t.Error("update vote_summary of completed job should be blocked")
			return
		}
		if !strings.Contains(err.Error(), "completed") {
			t.Errorf("error not actionable: %v", err)
		}
	})

	// Delete should be blocked.
	expectErrorAsTenant(t, conn, "imm-vote-c", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"DELETE FROM vote_summaries WHERE job_id = $1 AND source_id = $2 AND target_id = $3",
			"job-imm-vote-c-1", "src-1", "tgt-1")
		if err == nil {
			t.Error("delete vote_summary of completed job should be blocked")
			return
		}
		if !strings.Contains(err.Error(), "completed") {
			t.Errorf("error not actionable: %v", err)
		}
	})
}

func TestIntegration_Immutability_VoteSummary_PendingJob(t *testing.T) {
	setupTenant(t, "imm-vote-p", "Imm VoteP Corp")
	conn := appUserConn(t)

	execAsTenant(t, conn, "imm-vote-p", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'setup') ON CONFLICT DO NOTHING",
			"job-imm-vote-p-1", "imm-vote-p")
		if err != nil {
			t.Fatalf("insert job: %v", err)
		}
		_, err = tx.ExecContext(context.Background(),
			"INSERT INTO vote_summaries (job_id, source_id, target_id, consensus, confidence, viability, tenant_id) VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING",
			"job-imm-vote-p-1", "src-1", "tgt-1", "agree", 0.9, 0.8, "imm-vote-p")
		if err != nil {
			t.Fatalf("insert vote_summary: %v", err)
		}
	})

	// Update should succeed because parent job is pending.
	execAsTenant(t, conn, "imm-vote-p", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"UPDATE vote_summaries SET confidence = 0.75 WHERE job_id = $1 AND source_id = $2 AND target_id = $3",
			"job-imm-vote-p-1", "src-1", "tgt-1")
		if err != nil {
			t.Errorf("update vote_summary of pending job should succeed: %v", err)
		}
	})
}

func TestIntegration_Immutability_JobStage_CompletedJob(t *testing.T) {
	setupTenant(t, "imm-stage", "Imm Stage Corp")
	conn := appUserConn(t)

	execAsTenant(t, conn, "imm-stage", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'completed', 'setup') ON CONFLICT DO NOTHING",
			"job-imm-stage-1", "imm-stage")
		if err != nil {
			t.Fatalf("insert job: %v", err)
		}
		_, err = tx.ExecContext(context.Background(),
			"INSERT INTO job_stages (job_id, stage_name, status, tenant_id) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING",
			"job-imm-stage-1", "parse", "completed", "imm-stage")
		if err != nil {
			t.Fatalf("insert job_stage: %v", err)
		}
	})

	// Update should be blocked.
	expectErrorAsTenant(t, conn, "imm-stage", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"UPDATE job_stages SET status = 'failed' WHERE job_id = $1 AND stage_name = $2",
			"job-imm-stage-1", "parse")
		if err == nil {
			t.Error("update job_stage of completed job should be blocked")
			return
		}
		if !strings.Contains(err.Error(), "completed") {
			t.Errorf("error not actionable: %v", err)
		}
	})

	// Delete should be blocked.
	expectErrorAsTenant(t, conn, "imm-stage", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"DELETE FROM job_stages WHERE job_id = $1 AND stage_name = $2",
			"job-imm-stage-1", "parse")
		if err == nil {
			t.Error("delete job_stage of completed job should be blocked")
			return
		}
		if !strings.Contains(err.Error(), "completed") {
			t.Errorf("error not actionable: %v", err)
		}
	})
}

func TestIntegration_Immutability_BulkMixedStatus(t *testing.T) {
	setupTenant(t, "imm-bulk", "Imm Bulk Corp")
	conn := appUserConn(t)

	// Insert one pending and one completed job.
	execAsTenant(t, conn, "imm-bulk", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'setup') ON CONFLICT DO NOTHING",
			"job-imm-bulk-pend", "imm-bulk")
		if err != nil {
			t.Fatalf("insert pending: %v", err)
		}
		_, err = tx.ExecContext(context.Background(),
			"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'completed', 'setup') ON CONFLICT DO NOTHING",
			"job-imm-bulk-done", "imm-bulk")
		if err != nil {
			t.Fatalf("insert completed: %v", err)
		}
	})

	// Bulk update targeting both should fail entirely (trigger fires per-row).
	expectErrorAsTenant(t, conn, "imm-bulk", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"UPDATE jobs SET config = '{\"bulk\": true}'")
		if err == nil {
			t.Error("bulk update touching completed job should fail entirely")
			return
		}
		if !strings.Contains(err.Error(), "completed") {
			t.Errorf("error not actionable: %v", err)
		}
	})

	// Verify no partial success: the pending job should be unchanged.
	execAsTenant(t, conn, "imm-bulk", func(tx *sql.Tx) {
		var config sql.NullString
		err := tx.QueryRowContext(context.Background(),
			"SELECT config FROM jobs WHERE job_id = $1", "job-imm-bulk-pend").Scan(&config)
		if err != nil {
			t.Fatalf("scan config: %v", err)
		}
		if config.Valid && strings.Contains(config.String, "bulk") {
			t.Error("partial update leaked: pending job was modified despite completed job blocking")
		}
	})
}

// ---------------------------------------------------------------------------
// Tired Admin Scenarios
//
// Threat model: an operator or compromised service account attempts
// privileged operations that should be reserved for the migration role.
// app_user must not be able to reset completed state, bypass triggers,
// alter schema, or circumvent RLS. These tests verify defense-in-depth:
// even if application code has a bug, PostgreSQL itself blocks the abuse.
// ---------------------------------------------------------------------------

func TestIntegration_TiredAdmin_StatusReset(t *testing.T) {
	setupTenant(t, "tired-reset", "Tired Reset Corp")
	conn := appUserConn(t)

	execAsTenant(t, conn, "tired-reset", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'completed', 'setup') ON CONFLICT DO NOTHING",
			"job-tired-reset-1", "tired-reset")
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	})

	// Try to reset status back to pending.
	expectErrorAsTenant(t, conn, "tired-reset", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"UPDATE jobs SET status = 'pending' WHERE job_id = $1", "job-tired-reset-1")
		if err == nil {
			t.Error("resetting completed job to pending should be blocked")
			return
		}
		if !strings.Contains(err.Error(), "completed") {
			t.Errorf("error not actionable: %v", err)
		}
	})

	// Verify status unchanged.
	execAsTenant(t, conn, "tired-reset", func(tx *sql.Tx) {
		var status string
		err := tx.QueryRowContext(context.Background(),
			"SELECT status FROM jobs WHERE job_id = $1", "job-tired-reset-1").Scan(&status)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if status != "completed" {
			t.Errorf("status = %q after reset attempt, want completed", status)
		}
	})
}

func TestIntegration_TiredAdmin_BulkConfigUpdate(t *testing.T) {
	setupTenant(t, "tired-bulk", "Tired Bulk Corp")
	conn := appUserConn(t)

	execAsTenant(t, conn, "tired-bulk", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'setup') ON CONFLICT DO NOTHING",
			"job-tired-bulk-1", "tired-bulk")
		if err != nil {
			t.Fatalf("insert pending: %v", err)
		}
		_, err = tx.ExecContext(context.Background(),
			"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'completed', 'setup') ON CONFLICT DO NOTHING",
			"job-tired-bulk-2", "tired-bulk")
		if err != nil {
			t.Fatalf("insert completed: %v", err)
		}
	})

	// Bulk UPDATE all jobs should fail if any are completed.
	expectErrorAsTenant(t, conn, "tired-bulk", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"UPDATE jobs SET config = '{\"global\": true}'")
		if err == nil {
			t.Error("bulk config update should fail when any job is completed")
		}
	})
}

func TestIntegration_TiredAdmin_DeleteAndReinsert(t *testing.T) {
	setupTenant(t, "tired-reinst", "Tired Reinsert Corp")
	conn := appUserConn(t)

	execAsTenant(t, conn, "tired-reinst", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'completed', 'setup') ON CONFLICT DO NOTHING",
			"job-tired-reinst-1", "tired-reinst")
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	})

	// DELETE completed job should be blocked.
	expectErrorAsTenant(t, conn, "tired-reinst", func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"DELETE FROM jobs WHERE job_id = $1", "job-tired-reinst-1")
		if err == nil {
			t.Error("delete of completed job should be blocked")
			return
		}
		if !strings.Contains(err.Error(), "completed") {
			t.Errorf("error not actionable: %v", err)
		}
	})
}

func TestIntegration_TiredAdmin_TruncateTable(t *testing.T) {
	conn := appUserConn(t)
	ctx := context.Background()

	_, err := conn.ExecContext(ctx, "TRUNCATE TABLE jobs CASCADE")
	if err == nil {
		t.Error("app_user should not have TRUNCATE permission")
		return
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("unexpected error for TRUNCATE: %v", err)
	}
}

func TestIntegration_TiredAdmin_AlterTable(t *testing.T) {
	conn := appUserConn(t)
	ctx := context.Background()

	_, err := conn.ExecContext(ctx, "ALTER TABLE jobs ADD COLUMN hacked TEXT")
	if err == nil {
		t.Error("app_user should not have ALTER TABLE permission")
		return
	}
	if !strings.Contains(err.Error(), "permission denied") && !strings.Contains(err.Error(), "must be owner") {
		t.Errorf("unexpected error for ALTER TABLE: %v", err)
	}
}

func TestIntegration_TiredAdmin_DisableRLS(t *testing.T) {
	conn := appUserConn(t)
	ctx := context.Background()

	_, err := conn.ExecContext(ctx, "ALTER TABLE jobs DISABLE ROW LEVEL SECURITY")
	if err == nil {
		t.Error("app_user should not be able to disable RLS")
		return
	}
	if !strings.Contains(err.Error(), "permission denied") && !strings.Contains(err.Error(), "must be owner") {
		t.Errorf("unexpected error for DISABLE RLS: %v", err)
	}
}

func TestIntegration_TiredAdmin_DisableTrigger(t *testing.T) {
	conn := appUserConn(t)
	ctx := context.Background()

	_, err := conn.ExecContext(ctx, "ALTER TABLE jobs DISABLE TRIGGER jobs_immutable_update")
	if err == nil {
		t.Error("app_user should not be able to disable triggers")
		return
	}
	if !strings.Contains(err.Error(), "permission denied") && !strings.Contains(err.Error(), "must be owner") {
		t.Errorf("unexpected error for DISABLE TRIGGER: %v", err)
	}
}

func TestIntegration_TiredAdmin_CopyCommand(t *testing.T) {
	conn := appUserConn(t)
	ctx := context.Background()

	// COPY TO requires superuser or pg_read_server_files.
	_, err := conn.ExecContext(ctx, "COPY jobs TO '/tmp/exfil.csv' CSV")
	if err == nil {
		t.Error("app_user should not have COPY TO permission")
		return
	}
	// Error may mention "permission denied" or "must be superuser".
	errMsg := err.Error()
	if !strings.Contains(errMsg, "permission denied") && !strings.Contains(errMsg, "superuser") && !strings.Contains(errMsg, "pg_read_server_files") {
		t.Errorf("unexpected error for COPY: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Graph Lifecycle
// ---------------------------------------------------------------------------

func TestIntegration_GraphCreate(t *testing.T) {
	tenantID := "graph-create"
	setupTenant(t, tenantID, "Graph Create Corp")

	// Verify the graph was auto-created by the trigger.
	ctx := context.Background()
	var graphName string
	row := suPool.QueryRow(ctx,
		"SELECT name FROM ag_catalog.ag_graph WHERE name = $1",
		"crosscodex_"+tenantID)
	if err := row.Scan(&graphName); err != nil {
		t.Fatalf("graph not found after tenant insert: %v", err)
	}
	if graphName != "crosscodex_"+tenantID {
		t.Errorf("graph name = %q, want crosscodex_%s", graphName, tenantID)
	}
}

func TestIntegration_GraphDrop(t *testing.T) {
	tenantID := "graph-drop"
	setupTenant(t, tenantID, "Graph Drop Corp")

	ctx := context.Background()

	// Verify graph exists.
	var graphName string
	row := suPool.QueryRow(ctx,
		"SELECT name FROM ag_catalog.ag_graph WHERE name = $1",
		"crosscodex_"+tenantID)
	if err := row.Scan(&graphName); err != nil {
		t.Fatalf("graph not found before delete: %v", err)
	}

	// Delete the tenant (superuser, bypasses RLS).
	if err := suPool.Exec(ctx, "DELETE FROM tenants WHERE tenant_id = $1", tenantID); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}

	// Verify graph was dropped by the trigger.
	row = suPool.QueryRow(ctx,
		"SELECT name FROM ag_catalog.ag_graph WHERE name = $1",
		"crosscodex_"+tenantID)
	err := row.Scan(&graphName)
	if err == nil {
		t.Error("graph still exists after tenant deletion")
	}
}

func TestIntegration_TenantGraphName(t *testing.T) {
	conn := appUserConn(t)
	tenantID := "graph-name-fn"
	setupTenant(t, tenantID, "Graph Name Corp")

	execAsTenant(t, conn, tenantID, func(tx *sql.Tx) {
		var name string
		err := tx.QueryRowContext(context.Background(),
			"SELECT tenant_graph_name()").Scan(&name)
		if err != nil {
			t.Fatalf("tenant_graph_name(): %v", err)
		}
		expected := "crosscodex_" + tenantID
		if name != expected {
			t.Errorf("tenant_graph_name() = %q, want %q", name, expected)
		}
	})
}

func TestIntegration_AssertTenantGraph_Mismatch(t *testing.T) {
	conn := appUserConn(t)
	tenantID := "graph-assert"
	setupTenant(t, tenantID, "Graph Assert Corp")

	expectErrorAsTenant(t, conn, tenantID, func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"SELECT assert_tenant_graph($1)", "wrong_graph_name")
		if err == nil {
			t.Error("assert_tenant_graph with wrong name should fail")
			return
		}
		if !strings.Contains(err.Error(), "does not match") {
			t.Errorf("error should mention mismatch: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// TenantPool
// ---------------------------------------------------------------------------

func TestIntegration_TenantPool_BeginSetsTenant(t *testing.T) {
	tenantID := "tpool-tenant"
	setupTenant(t, tenantID, "TPool Tenant Corp")

	pool, err := db.NewPool(db.PoolConfig{
		DSN:          appUserDSN(),
		MaxOpenConns: 2,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	tp := db.NewTenantPool(pool)
	ctx := db.ContextWithTenant(context.Background(), tenantID)

	tx, err := tp.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	row := tx.QueryRow(context.Background(), "SELECT current_setting('app.current_tenant', true)")
	var got string
	if err := row.Scan(&got); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got != tenantID {
		t.Errorf("current_setting = %q, want %q", got, tenantID)
	}
}

func TestIntegration_TenantPool_BeginSetsUser(t *testing.T) {
	tenantID := "tpool-user"
	userID := "test-alice"
	setupTenant(t, tenantID, "TPool User Corp")

	pool, err := db.NewPool(db.PoolConfig{
		DSN:          appUserDSN(),
		MaxOpenConns: 2,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	tp := db.NewTenantPool(pool)
	ctx := db.ContextWithTenant(context.Background(), tenantID)
	ctx = db.ContextWithUser(ctx, userID)

	tx, err := tp.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	row := tx.QueryRow(context.Background(), "SELECT current_setting('app.current_user', true)")
	var got string
	if err := row.Scan(&got); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got != userID {
		t.Errorf("current_setting = %q, want %q", got, userID)
	}
}

// ---------------------------------------------------------------------------
// Role Isolation
//
// The three-role model (postgres / app_user / graph_user) enforces a hard
// boundary between relational data and graph data at the PostgreSQL level.
// Even if application code has a bug — a SQL injection, a misrouted query,
// a confused-deputy attack — PostgreSQL itself blocks cross-boundary access.
//
//   app_user  → relational tables (public schema) with RLS. Zero graph access.
//   graph_user → per-tenant graph schemas + ag_catalog. Zero relational access.
//   postgres  → everything. Used only for migrations, never at runtime.
//
// The tests below are organized into two groups:
//
//   Positive tests: verify that each role CAN do what it is designed to do.
//     These catch grant regressions — a migration that accidentally revokes
//     a needed privilege would break the positive test first.
//
//   Negative tests: verify that each role CANNOT cross the boundary.
//     These catch over-granting — a migration that accidentally grants
//     ag_catalog access to app_user would break the negative test.
//
// Together they form an executable security specification. Each test comment
// states what it proves, what breaks if the assertion fails, and which
// PostgreSQL mechanism (GRANT/REVOKE, schema ownership, RLS) enforces it.
// ---------------------------------------------------------------------------

// --- Positive: app_user can do its job ---

// TestIntegration_AppUser_CanQueryRelationalData verifies app_user can
// SELECT from tenants and jobs with proper tenant context. This proves
// migration 007's DML grants (SELECT/INSERT/UPDATE/DELETE on public-schema
// tables) are in effect. If this breaks, app_user has lost the ability to
// do its primary job: serve relational queries for the application layer.
// Enforced by: GRANT SELECT, INSERT, UPDATE, DELETE on public tables to
// app_user (migration 007).
func TestIntegration_AppUser_CanQueryRelationalData(t *testing.T) {
	tenantID := "role-appuser-dml"
	setupTenant(t, tenantID, "AppUser DML Corp")
	conn := appUserConn(t)

	execAsTenant(t, conn, tenantID, func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(),
			"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'pending', 'test') ON CONFLICT DO NOTHING",
			"job-role-dml-1", tenantID)
		if err != nil {
			t.Fatalf("app_user INSERT into jobs failed: %v", err)
		}

		var count int
		if err := tx.QueryRowContext(context.Background(),
			"SELECT count(*) FROM jobs WHERE job_id = $1", "job-role-dml-1").Scan(&count); err != nil {
			t.Fatalf("app_user SELECT from jobs failed: %v", err)
		}
		if count != 1 {
			t.Errorf("app_user sees %d rows, want 1", count)
		}
	})
}

// --- Positive: graph_user can do its job ---

// TestIntegration_GraphUser_CanQueryAgCatalog verifies graph_user can
// SELECT from ag_catalog.ag_graph. This proves migration 009's GRANT
// USAGE ON SCHEMA ag_catalog + GRANT SELECT ON ALL TABLES are in effect.
// If this breaks, the graph client cannot discover or verify tenant
// graphs, blocking all graph operations. Enforced by: GRANT USAGE on
// ag_catalog schema and GRANT SELECT on ag_catalog tables to graph_user
// (migration 009).
func TestIntegration_GraphUser_CanQueryAgCatalog(t *testing.T) {
	tenantID := "role-graphcat"
	setupTenant(t, tenantID, "Graph Catalog Corp")
	conn := graphUserConn(t)
	ctx := context.Background()

	var count int
	err := conn.QueryRowContext(ctx,
		"SELECT count(*) FROM ag_catalog.ag_graph WHERE name = $1",
		"crosscodex_"+tenantID).Scan(&count)
	if err != nil {
		t.Fatalf("graph_user SELECT from ag_catalog.ag_graph failed: %v", err)
	}
	if count != 1 {
		t.Errorf("graph_user sees %d graphs for tenant %s, want 1", count, tenantID)
	}
}

// TestIntegration_GraphUser_CanAccessGraphSchema verifies graph_user can
// SELECT from per-tenant graph schema tables (e.g. _ag_label_vertex).
// AGE creates these tables when a graph is created; the provisioning
// trigger transfers ownership to graph_user. If this breaks, the graph
// client cannot read or write vertices/edges, which is its entire purpose.
// Enforced by: ALTER SCHEMA/TABLE/SEQUENCE OWNER TO graph_user in the
// provisioning trigger (migration 009).
func TestIntegration_GraphUser_CanAccessGraphSchema(t *testing.T) {
	tenantID := "role-graphschema"
	setupTenant(t, tenantID, "Graph Schema Corp")
	conn := graphUserConn(t)
	ctx := context.Background()

	schemaName := "crosscodex_" + tenantID
	query := fmt.Sprintf(
		`SELECT count(*) FROM "%s"._ag_label_vertex`, schemaName)
	var count int
	if err := conn.QueryRowContext(ctx, query).Scan(&count); err != nil {
		t.Fatalf("graph_user SELECT from %s._ag_label_vertex failed: %v", schemaName, err)
	}
}

// --- Negative: app_user blocked from graph layer ---

// TestIntegration_AppUser_CannotReadAgCatalog verifies app_user has no
// access to the ag_catalog schema. Without this boundary, a bug in
// relational code could enumerate all tenant graphs, leaking the tenant
// directory across the graph/relational privilege boundary. Enforced by
// REVOKE: migration 009 grants ag_catalog access to graph_user only;
// app_user has no USAGE on the ag_catalog schema.
func TestIntegration_AppUser_CannotReadAgCatalog(t *testing.T) {
	conn := appUserConn(t)
	ctx := context.Background()

	_, err := conn.ExecContext(ctx, "SELECT count(*) FROM ag_catalog.ag_graph")
	if err == nil {
		t.Error("app_user should not be able to read ag_catalog.ag_graph")
		return
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("expected 'permission denied', got: %v", err)
	}
}

// TestIntegration_AppUser_CannotReadGraphSchema verifies app_user cannot
// SELECT from per-tenant graph schema tables. Without this boundary, a
// SQL injection in relational code could read graph vertices/edges,
// exfiltrating compliance relationship data that should only be accessible
// through the graph API. Enforced by: app_user has no USAGE on per-tenant
// graph schemas (owned by graph_user); PostgreSQL denies access by default
// to schemas the role has no USAGE grant on.
func TestIntegration_AppUser_CannotReadGraphSchema(t *testing.T) {
	tenantID := "role-appgraph"
	setupTenant(t, tenantID, "AppGraph Corp")
	conn := appUserConn(t)
	ctx := context.Background()

	schemaName := "crosscodex_" + tenantID
	query := fmt.Sprintf(
		`SELECT count(*) FROM "%s"._ag_label_vertex`, schemaName)
	_, err := conn.ExecContext(ctx, query)
	if err == nil {
		t.Errorf("app_user should not be able to read %s._ag_label_vertex", schemaName)
		return
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("expected 'permission denied', got: %v", err)
	}
}

// TestIntegration_AppUser_CannotCallCypher verifies app_user cannot
// EXECUTE ag_catalog.cypher(). Without this boundary, a bug in relational
// code could run arbitrary Cypher queries against tenant graphs — reading,
// creating, or deleting vertices and edges. Enforced by: app_user has no
// EXECUTE on ag_catalog functions (migration 009 grants EXECUTE to
// graph_user only).
func TestIntegration_AppUser_CannotCallCypher(t *testing.T) {
	tenantID := "role-appnocypher"
	setupTenant(t, tenantID, "AppNoCypher Corp")
	conn := appUserConn(t)
	ctx := context.Background()

	query := fmt.Sprintf(
		"SELECT * FROM ag_catalog.cypher('crosscodex_%s', $$ MATCH (n) RETURN n $$) AS (v agtype)",
		tenantID)
	_, err := conn.ExecContext(ctx, query)
	if err == nil {
		t.Error("app_user should not be able to call ag_catalog.cypher()")
		return
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, "permission denied") {
		t.Errorf("expected 'permission denied', got: %v", err)
	}
}

// TestIntegration_AppUser_CannotCreateGraph verifies app_user cannot
// EXECUTE ag_catalog.create_graph(). Without this boundary, compromised
// relational code could create rogue graphs outside the tenant
// provisioning flow, bypassing ownership transfer and audit controls.
// Enforced by: app_user has no EXECUTE on ag_catalog functions
// (migration 009 grants EXECUTE to graph_user only).
func TestIntegration_AppUser_CannotCreateGraph(t *testing.T) {
	conn := appUserConn(t)
	ctx := context.Background()

	_, err := conn.ExecContext(ctx, "SELECT ag_catalog.create_graph('rogue_graph')")
	if err == nil {
		t.Error("app_user should not be able to call ag_catalog.create_graph()")
		return
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, "permission denied") {
		t.Errorf("expected 'permission denied', got: %v", err)
	}
}

// --- Negative: graph_user blocked from relational layer ---

// TestIntegration_GraphUser_CannotReadTenants verifies graph_user cannot
// SELECT from the tenants table. Without this boundary, a bug in graph
// code could enumerate all tenants — including ones the graph_user has no
// graph for — leaking the tenant directory. Enforced by: graph_user has
// no privileges on public-schema tables (only app_user has DML grants).
func TestIntegration_GraphUser_CannotReadTenants(t *testing.T) {
	conn := graphUserConn(t)
	ctx := context.Background()

	_, err := conn.ExecContext(ctx, "SELECT count(*) FROM tenants")
	if err == nil {
		t.Error("graph_user should not be able to read tenants table")
		return
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("expected 'permission denied', got: %v", err)
	}
}

// TestIntegration_GraphUser_CannotReadJobs verifies graph_user cannot
// SELECT from the jobs table. Without this boundary, a bug in graph code
// could read job configurations, statuses, and user identifiers —
// sensitive operational data outside the graph domain. Enforced by:
// graph_user has no privileges on public-schema tables.
func TestIntegration_GraphUser_CannotReadJobs(t *testing.T) {
	conn := graphUserConn(t)
	ctx := context.Background()

	_, err := conn.ExecContext(ctx, "SELECT count(*) FROM jobs")
	if err == nil {
		t.Error("graph_user should not be able to read jobs table")
		return
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("expected 'permission denied', got: %v", err)
	}
}

// TestIntegration_GraphUser_CannotInsertTenants verifies graph_user
// cannot INSERT into the tenants table. Without this boundary,
// compromised graph code could provision rogue tenants, creating new
// graph schemas and bypassing whatever onboarding controls the
// application enforces. Enforced by: graph_user has no INSERT privilege
// on public-schema tables.
func TestIntegration_GraphUser_CannotInsertTenants(t *testing.T) {
	conn := graphUserConn(t)
	ctx := context.Background()

	_, err := conn.ExecContext(ctx,
		"INSERT INTO tenants (tenant_id, display_name) VALUES ('rogue-tenant', 'Rogue Corp')")
	if err == nil {
		t.Error("graph_user should not be able to insert into tenants")
		return
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("expected 'permission denied', got: %v", err)
	}
}

// TestIntegration_GraphUser_CannotTruncateRelational verifies graph_user
// cannot TRUNCATE relational tables. Without this boundary, compromised
// graph code could wipe all job history, classifications, and vote
// summaries in a single statement. Enforced by: TRUNCATE requires table
// ownership or explicit TRUNCATE grant, neither of which graph_user has
// on public-schema tables.
func TestIntegration_GraphUser_CannotTruncateRelational(t *testing.T) {
	conn := graphUserConn(t)
	ctx := context.Background()

	_, err := conn.ExecContext(ctx, "TRUNCATE TABLE jobs CASCADE")
	if err == nil {
		t.Error("graph_user should not be able to truncate jobs")
		return
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("expected 'permission denied', got: %v", err)
	}
}

// TestIntegration_GraphUser_CannotAlterRelational verifies graph_user
// cannot ALTER relational tables. Without this boundary, compromised
// graph code could add columns, drop constraints, or disable RLS on
// relational tables — subverting the entire tenant isolation model.
// Enforced by: ALTER TABLE requires table ownership; public-schema
// tables are owned by postgres, not graph_user.
func TestIntegration_GraphUser_CannotAlterRelational(t *testing.T) {
	conn := graphUserConn(t)
	ctx := context.Background()

	_, err := conn.ExecContext(ctx, "ALTER TABLE jobs ADD COLUMN hacked TEXT")
	if err == nil {
		t.Error("graph_user should not be able to alter relational tables")
		return
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, "permission denied") && !strings.Contains(errMsg, "must be owner") {
		t.Errorf("expected 'permission denied' or 'must be owner', got: %v", err)
	}
}
