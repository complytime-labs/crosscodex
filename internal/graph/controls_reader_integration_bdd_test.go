//go:build integration

package graph_test

// Suite bootstrap lives in resolver_bdd_test.go — do NOT add RunSpecs here.

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// The reader runs as the restricted app_user role, never the superuser, so
// these specs prove Postgres itself (RLS plus the reader's tenant binding)
// keeps one tenant's controls away from another.
var _ = Describe("PGControlsReader tenant isolation", Ordered, func() {
	const (
		tenantA   = "graph-ctlr-tenant-a"
		tenantB   = "graph-ctlr-tenant-b"
		catalogA  = "graphctlrcatalog-a"
		catalogB  = "graphctlrcatalog-b"
		suiteName = "graph controls reader"
	)

	var (
		reader *graph.PGControlsReader
		ctxA   context.Context
		ctxB   context.Context
	)

	BeforeAll(func() {
		ctx := context.Background()
		suDSN := os.Getenv("TEST_DATABASE_DSN")
		if suDSN == "" {
			Fail("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		migrator, err := db.NewMigrator(suDSN)
		Expect(err).NotTo(HaveOccurred(), "failed to create migrator")
		Expect(migrator.Up(ctx)).To(Succeed(), "failed to run migrations")
		Expect(migrator.Close()).To(Succeed())

		suPool, err := db.NewPool(db.PoolConfig{DSN: suDSN, MaxOpenConns: 2})
		Expect(err).NotTo(HaveOccurred(), "failed to create superuser pool")
		DeferCleanup(suPool.Close)

		appPassword, err := dbtest.RolePassword(dbtest.AppUserPasswordEnv)
		Expect(err).NotTo(HaveOccurred(), "app_user password")
		stmt, err := dbtest.AlterRolePasswordSQL("app_user", appPassword)
		Expect(err).NotTo(HaveOccurred(), "build ALTER ROLE app_user")
		Expect(suPool.Exec(ctx, stmt)).To(Succeed(), "failed to set app_user password")

		// Seed as the superuser. catalog_id is globally unique and each
		// controls row references its catalog's (catalog_id, tenant_id), so
		// tenant B cannot hold a catalog with tenant A's ID; B gets its own.
		Expect(db.EnsureTenant(ctx, suPool, tenantA, suiteName+" A")).To(Succeed())
		Expect(db.EnsureTenant(ctx, suPool, tenantB, suiteName+" B")).To(Succeed())
		for _, c := range []struct{ tenantID, catalogID string }{{tenantA, catalogA}, {tenantB, catalogB}} {
			Expect(suPool.Exec(ctx,
				`INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path)
				 VALUES ($1, $2, 'controls reader', '1.0', 'oscal', 'test') ON CONFLICT DO NOTHING`,
				c.catalogID, c.tenantID)).To(Succeed())
		}
		seedControl := func(tenantID, catalogID, identifier, title, statement, class, parentID string) {
			Expect(suPool.Exec(ctx,
				`INSERT INTO controls (tenant_id, control_id, catalog_id, identifier, title, statement, class, parent_id)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING`,
				tenantID, catalogID+"/"+identifier, catalogID, identifier, title, statement, class, parentID)).To(Succeed())
		}
		seedControl(tenantA, catalogA, "ac", "Access Control", "", "family", "")
		seedControl(tenantA, catalogA, "ac-1", "Policy and Procedures", "Develop a policy.", "SP800-53", "ac")
		seedControl(tenantB, catalogB, "ac", "Tenant B Access Control", "", "family", "")

		u, err := url.Parse(suDSN)
		Expect(err).NotTo(HaveOccurred(), "bad TEST_DATABASE_DSN")
		u.User = url.UserPassword("app_user", appPassword)
		appDSN := u.String()

		roleDB, err := sql.Open("pgx", appDSN)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(roleDB.Close)
		Expect(db.CheckRestrictedRole(ctx, roleDB, "TEST_DATABASE_DSN as app_user")).To(Succeed(),
			"the reader must run as a role that cannot bypass RLS")

		appPool, err := db.NewPool(db.PoolConfig{DSN: appDSN, MaxOpenConns: 2})
		Expect(err).NotTo(HaveOccurred(), "failed to create app_user pool")
		DeferCleanup(appPool.Close)
		reader = graph.NewPGControlsReader(db.NewTenantPool(appPool))

		ctxA, err = tenant.WithTenant(ctx, tenantA)
		Expect(err).NotTo(HaveOccurred())
		ctxB, err = tenant.WithTenant(ctx, tenantB)
		Expect(err).NotTo(HaveOccurred())
	})

	It("lists the ctx tenant's controls with every column mapped", func() {
		got, err := reader.ListCatalogControls(ctxA, catalogA)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal([]graph.CatalogControl{
			{ControlID: catalogA + "/ac", CatalogID: catalogA, Identifier: "ac",
				Title: "Access Control", Class: "family"},
			{ControlID: catalogA + "/ac-1", CatalogID: catalogA, Identifier: "ac-1",
				Title: "Policy and Procedures", Statement: "Develop a policy.", Class: "SP800-53", ParentID: "ac"},
		}))
	})

	It("returns no rows when another tenant asks for the catalog", func() {
		got, err := reader.ListCatalogControls(ctxB, catalogA)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())
	})

	It("still lists the other tenant's own catalog, so the empty result is isolation", func() {
		got, err := reader.ListCatalogControls(ctxB, catalogB)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		Expect(got[0].Title).To(Equal("Tenant B Access Control"))
	})

	It("refuses a ctx without a tenant", func() {
		_, err := reader.ListCatalogControls(context.Background(), catalogA)
		Expect(errors.Is(err, tenant.ErrNoTenant)).To(BeTrue(), "got %v", err)
	})
})
