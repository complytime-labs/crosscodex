//go:build integration

package pipeline

import (
	"context"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
)

// integrationDB is a migrated test database: a superuser pool for seed
// writes and checks that must bypass RLS, and the restricted app_user role's
// pool and tenant-scoped connection over it, which the code under test uses
// so RLS and the role's grants apply exactly as in production.
type integrationDB struct {
	suDSN      string
	suPool     db.Pool
	appPool    db.Pool
	tenantConn db.TenantConnection
}

// openIntegrationDB migrates TEST_DATABASE_DSN, sets the app_user password
// and opens both pools, closing them when the current spec, or the BeforeAll
// that called it, ends. It skips the spec when TEST_DATABASE_DSN is unset.
// appConns bounds the app_user pool. Offset 1 so a failure is reported at
// the caller's line.
func openIntegrationDB(ctx context.Context, appConns int) *integrationDB {
	suDSN := os.Getenv("TEST_DATABASE_DSN")
	if suDSN == "" {
		Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
	}

	migrator, err := db.NewMigrator(suDSN)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "create migrator")
	ExpectWithOffset(1, migrator.Up(ctx)).To(Succeed(), "run migrations")
	ExpectWithOffset(1, migrator.Close()).To(Succeed(), "close migrator")

	appPassword, err := dbtest.RolePassword(dbtest.AppUserPasswordEnv)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "app_user password")
	ExpectWithOffset(1, dbtest.SetRolePassword(ctx, suDSN, "app_user", appPassword)).To(Succeed(), "set app_user password")

	suPool, err := db.NewPool(db.PoolConfig{DSN: suDSN, MaxOpenConns: 5, Extensions: []string{"age", "vector"}})
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "create superuser pool")
	DeferCleanup(suPool.Close)

	appDSN, err := dbtest.RoleDSN(suDSN, "app_user", appPassword)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "build app_user DSN")
	appPool, err := db.NewPool(db.PoolConfig{DSN: appDSN, MaxOpenConns: appConns})
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "create app_user pool")
	DeferCleanup(appPool.Close)

	return &integrationDB{suDSN: suDSN, suPool: suPool, appPool: appPool, tenantConn: db.NewTenantPool(appPool)}
}
