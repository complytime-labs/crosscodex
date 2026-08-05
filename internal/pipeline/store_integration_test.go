//go:build integration

package pipeline_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/google/uuid"
)

func TestPGStore(t *testing.T) {
	ctx := context.Background()

	// 1. Check TEST_DATABASE_DSN
	suDSN := os.Getenv("TEST_DATABASE_DSN")
	if suDSN == "" {
		t.Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
	}

	// 2. Run migrations (idempotent)
	migrator, err := db.NewMigrator(suDSN)
	if err != nil {
		t.Fatalf("failed to create migrator: %v", err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatalf("failed to run migrations: %v", err)
	}
	if err := migrator.Close(); err != nil {
		t.Fatalf("failed to close migrator: %v", err)
	}

	// 3. Set app_user password (idempotent)
	adminDB, err := sql.Open("pgx", suDSN)
	if err != nil {
		t.Fatalf("failed to open admin connection: %v", err)
	}
	_, err = adminDB.ExecContext(ctx, "ALTER ROLE app_user WITH PASSWORD 'apppass'")
	if err != nil {
		t.Fatalf("failed to set app_user password: %v", err)
	}
	if err := adminDB.Close(); err != nil {
		t.Fatalf("failed to close admin connection: %v", err)
	}

	// 4. Create superuser pool
	suPool, err := db.NewPool(db.PoolConfig{
		DSN:          suDSN,
		MaxOpenConns: 5,
		Extensions:   []string{"age", "vector"},
	})
	if err != nil {
		t.Fatalf("failed to create superuser pool: %v", err)
	}
	defer suPool.Close()

	// 5. Build app_user DSN and create app pool
	appDSN, err := appUserDSN(suDSN)
	if err != nil {
		t.Fatalf("failed to build app_user DSN: %v", err)
	}
	appPool, err := db.NewPool(db.PoolConfig{
		DSN:          appDSN,
		MaxOpenConns: 2,
	})
	if err != nil {
		t.Fatalf("failed to create app_user pool: %v", err)
	}
	defer appPool.Close()

	tenantConn := db.NewTenantPool(appPool)

	// 6. Provision tenant (unique per run)
	tenantID := fmt.Sprintf("pipeline-it-%s", uuid.New().String())
	if err := db.EnsureTenant(ctx, suPool, tenantID, "pipeline-it"); err != nil {
		t.Fatalf("failed to ensure tenant: %v", err)
	}

	// 7. Set tenant context
	ctx, err = tenant.WithTenant(ctx, tenantID)
	if err != nil {
		t.Fatalf("failed to set tenant context: %v", err)
	}

	// 8. Create PGStore
	store := pipeline.NewPGStore(tenantConn, suPool)

	// 9. Build a valid Job
	now := time.Now().UTC()
	job := &pipeline.Job{
		JobID:     uuid.New().String(),
		TenantID:  tenantID,
		Status:    pipeline.JobStatusPending,
		Config:    []byte(`{}`),
		CreatedBy: "integration-test",
		CreatedAt: now,
		UpdatedAt: now,
	}

	// 10. CreateJob — THIS WILL FAIL with SQLSTATE 42601 before the fix
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	// 11. GetJob — verify round-trip
	got, err := store.GetJob(ctx, job.JobID)
	if err != nil {
		t.Fatalf("GetJob failed: %v", err)
	}
	if got.JobID != job.JobID {
		t.Errorf("JobID mismatch: got %q, want %q", got.JobID, job.JobID)
	}
	if got.TenantID != job.TenantID {
		t.Errorf("TenantID mismatch: got %q, want %q", got.TenantID, job.TenantID)
	}
	if got.Status != job.Status {
		t.Errorf("Status mismatch: got %q, want %q", got.Status, job.Status)
	}
}

// appUserDSN swaps userinfo to app_user:apppass
func appUserDSN(suDSN string) (string, error) {
	u, err := url.Parse(suDSN)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword("app_user", "apppass")
	return u.String(), nil
}
