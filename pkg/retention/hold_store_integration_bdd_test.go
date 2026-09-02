//go:build integration

package retention_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/retention"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// holdIntegPool is the app_user pool for hold store integration tests.
// Initialised by BeforeSuite; closed by AfterSuite.
var holdIntegPool db.Pool

var _ = BeforeSuite(func() {
	suDSN := os.Getenv("TEST_DATABASE_DSN")
	if suDSN == "" {
		Fail("TEST_DATABASE_DSN not set — run: task test:integration:db")
	}

	ctx := context.Background()

	// Run migrations as superuser (idempotent — ErrNoChange handled internally).
	migrator, err := db.NewMigrator(suDSN)
	Expect(err).NotTo(HaveOccurred(), "create migrator")
	Expect(migrator.Up(ctx)).To(Succeed(), "run migrations")
	Expect(migrator.Close()).To(Succeed(), "close migrator")

	// Set the app_user password so pool authentication works.
	// This mirrors the SynchronizedBeforeSuite in pkg/db/db_integration_bdd_test.go.
	adminDB, err := sql.Open("pgx", suDSN)
	Expect(err).NotTo(HaveOccurred(), "open admin connection")
	_, err = adminDB.ExecContext(ctx, "ALTER ROLE app_user PASSWORD 'apppass'")
	Expect(err).NotTo(HaveOccurred(), "set app_user password")
	Expect(adminDB.Close()).To(Succeed(), "close admin connection")

	holdIntegPool, err = db.NewPool(db.PoolConfig{
		DSN:          holdAppUserDSN(suDSN),
		MaxOpenConns: 3,
	})
	Expect(err).NotTo(HaveOccurred(), "create app_user integration pool")
})

var _ = AfterSuite(func() {
	if holdIntegPool != nil {
		holdIntegPool.Close() //nolint:errcheck
	}
})

// holdAppUserDSN rewrites the superuser DSN to use app_user credentials.
// All TLS parameters are preserved since only the userinfo component changes.
func holdAppUserDSN(suDSN string) string {
	u, err := url.Parse(suDSN)
	if err != nil {
		panic(fmt.Sprintf("bad TEST_DATABASE_DSN: %v", err))
	}
	u.User = url.UserPassword("app_user", "apppass")
	return u.String()
}

var _ = Describe("pgHoldStore", Ordered, func() {
	var store retention.HoldStore
	var ctx context.Context
	const holdName = "integ-hold-alpha"

	BeforeAll(func() {
		store = retention.NewPgHoldStore(db.NewTenantPool(holdIntegPool))
		var err error
		ctx, err = tenant.WithTenant(context.Background(), "integ-tenant")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		// Best-effort cleanup so repeated test runs do not accumulate rows.
		// Under RLS the DELETE must run inside a tenant-scoped transaction;
		// an unscoped app_user delete would match zero rows.
		cleanupCtx, err := tenant.WithTenant(context.Background(), "integ-tenant")
		Expect(err).NotTo(HaveOccurred())
		tx, err := db.NewTenantPool(holdIntegPool).Begin(cleanupCtx)
		if err == nil {
			_ = tx.Exec(cleanupCtx, "DELETE FROM public.retention_holds WHERE name = $1", holdName)
			_ = tx.Commit()
		}
	})

	Describe("Create then List", Ordered, func() {
		var created retention.Hold

		It("creates a hold and returns it with a database-assigned ID", func() {
			cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			h := retention.Hold{
				Name:      holdName,
				CreatedBy: "test-runner",
				Scope: retention.HoldScope{
					CreatedBefore: &cutoff,
					TenantID:      "integ-tenant",
				},
			}
			var err error
			created, err = store.Create(ctx, h)
			Expect(err).NotTo(HaveOccurred())
			Expect(created.ID).NotTo(BeEmpty(), "database must assign a UUID")
			Expect(created.Name).To(Equal(holdName))
			Expect(created.CreatedBy).To(Equal("test-runner"))
			Expect(created.Scope.TenantID).To(Equal("integ-tenant"))
			Expect(created.Scope.CreatedBefore).NotTo(BeNil())
			Expect(*created.Scope.CreatedBefore).To(BeTemporally("==", cutoff))
			Expect(created.ReleasedAt).To(BeNil())
		})

		It("shows the hold in List", func() {
			holds, err := store.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			var names []string
			for _, h := range holds {
				names = append(names, h.Name)
			}
			Expect(names).To(ContainElement(holdName))
		})

		It("shows the hold in Active", func() {
			holds, err := store.Active(ctx, time.Now())
			Expect(err).NotTo(HaveOccurred())
			var names []string
			for _, h := range holds {
				names = append(names, h.Name)
			}
			Expect(names).To(ContainElement(holdName))
		})
	})

	Describe("Release", Ordered, func() {
		It("releases the hold without error", func() {
			Expect(store.Release(ctx, holdName, "test-runner")).To(Succeed())
		})

		It("excludes the released hold from Active", func() {
			holds, err := store.Active(ctx, time.Now())
			Expect(err).NotTo(HaveOccurred())
			var names []string
			for _, h := range holds {
				names = append(names, h.Name)
			}
			Expect(names).NotTo(ContainElement(holdName))
		})

		It("still shows the released hold in List (audit trail)", func() {
			holds, err := store.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			var found *retention.Hold
			for i := range holds {
				if holds[i].Name == holdName {
					found = &holds[i]
					break
				}
			}
			Expect(found).NotTo(BeNil(), "released hold must remain in List for audit")
			Expect(found.ReleasedAt).NotTo(BeNil())
			Expect(found.ReleasedBy).To(Equal("test-runner"))
		})
	})
})

var _ = Describe("retention_holds RLS", Ordered, func() {
	var storeA, storeB retention.HoldStore
	var ctxA, ctxB context.Context
	const nameA = "integ-hold-beta"

	BeforeAll(func() {
		tp := db.NewTenantPool(holdIntegPool)
		storeA, storeB = retention.NewPgHoldStore(tp), retention.NewPgHoldStore(tp)
		var err error
		ctxA, err = tenant.WithTenant(context.Background(), "tenant-a")
		Expect(err).NotTo(HaveOccurred())
		ctxB, err = tenant.WithTenant(context.Background(), "tenant-b")
		Expect(err).NotTo(HaveOccurred())
		_, err = storeA.Create(ctxA, retention.Hold{
			Name: nameA, CreatedBy: "a", Scope: retention.HoldScope{TenantID: "tenant-a"},
		})
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		tx, err := db.NewTenantPool(holdIntegPool).Begin(ctxA)
		if err == nil {
			_ = tx.Exec(ctxA, "DELETE FROM public.retention_holds WHERE name = $1", nameA)
			_ = tx.Commit()
		}
	})

	It("hides tenant-a's hold from tenant-b List", func() {
		holds, err := storeB.List(ctxB)
		Expect(err).NotTo(HaveOccurred())
		for _, h := range holds {
			Expect(h.Name).NotTo(Equal(nameA))
		}
	})

	It("hides tenant-a's hold from tenant-b Active", func() {
		holds, err := storeB.Active(ctxB, time.Now())
		Expect(err).NotTo(HaveOccurred())
		for _, h := range holds {
			Expect(h.Name).NotTo(Equal(nameA))
		}
	})

	It("does not let tenant-b release tenant-a's hold", func() {
		Expect(storeB.Release(ctxB, nameA, "b")).To(Succeed()) // affects 0 rows under RLS
		holds, err := storeA.Active(ctxA, time.Now())
		Expect(err).NotTo(HaveOccurred())
		var stillActive bool
		for _, h := range holds {
			if h.Name == nameA {
				stillActive = true
			}
		}
		Expect(stillActive).To(BeTrue(), "tenant-a's hold must remain active")
	})

	It("rejects creating a hold for another tenant (WITH CHECK)", func() {
		_, err := storeB.Create(ctxB, retention.Hold{
			Name: "integ-hold-cross", CreatedBy: "b",
			Scope: retention.HoldScope{TenantID: "tenant-a"},
		})
		Expect(err).To(HaveOccurred())
	})
})
