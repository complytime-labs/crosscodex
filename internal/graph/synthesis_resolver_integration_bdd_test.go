//go:build integration

package graph_test

// Suite bootstrap lives in resolver_bdd_test.go — do NOT add RunSpecs here.

import (
	"context"
	"database/sql"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// The resolver runs as the restricted app_user role, never the superuser, so
// these specs prove Postgres itself (RLS plus the resolver's tenant binding)
// keeps one tenant's vote summaries away from another.
var _ = Describe("PGResolver synthesis stage tenant isolation", Ordered, func() {
	const (
		tenantA   = "graph-synth-tenant-a"
		tenantB   = "graph-synth-tenant-b"
		jobA      = "graph-synth-job-a"
		jobB      = "graph-synth-job-b"
		suiteName = "graph synthesis resolver"
	)

	var (
		resolver *graph.PGResolver
		ctxA     context.Context
		ctxB     context.Context
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
		Expect(dbtest.SetRolePassword(ctx, suDSN, "app_user", appPassword)).To(Succeed(), "failed to set app_user password")

		Expect(db.EnsureTenant(ctx, suPool, tenantA, suiteName+" A")).To(Succeed())
		Expect(db.EnsureTenant(ctx, suPool, tenantB, suiteName+" B")).To(Succeed())
		for _, j := range []struct{ tenantID, jobID string }{{tenantA, jobA}, {tenantB, jobB}} {
			Expect(suPool.Exec(ctx,
				`INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'running', 'test') ON CONFLICT DO NOTHING`,
				j.jobID, j.tenantID)).To(Succeed())
		}
		seedVote := func(tenantID, jobID, source, target, analyzer, consensus string, confidence, viability float64) {
			Expect(suPool.Exec(ctx,
				`INSERT INTO vote_summaries (job_id, source_id, target_id, analyzer_name, consensus, confidence, viability, tenant_id)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING`,
				jobID, source, target, analyzer, consensus, confidence, viability, tenantID)).To(Succeed())
		}
		seedVote(tenantA, jobA, "c1", "c2", "relationship", "EQUIVALENT", 0.9, 0.7)
		seedVote(tenantA, jobA, "c2", "c3", "relationship", "SUBSET_OF", 0.8, 0)
		seedVote(tenantA, jobA, "c1", "c2", "requires", "REQUIRES", 0.95, 0.5)
		seedVote(tenantB, jobB, "c1", "c2", "relationship", "EQUIVALENT", 0.6, 0.4)

		appDSN, err := dbtest.RoleDSN(suDSN, "app_user", appPassword)
		Expect(err).NotTo(HaveOccurred(), "app_user DSN")
		roleDB, err := sql.Open("pgx", appDSN)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(roleDB.Close)
		Expect(db.CheckRestrictedRole(ctx, roleDB, "TEST_DATABASE_DSN as app_user")).To(Succeed(),
			"the resolver must run as a role that cannot bypass RLS")

		appPool, err := db.NewPool(db.PoolConfig{DSN: appDSN, MaxOpenConns: 2})
		Expect(err).NotTo(HaveOccurred(), "failed to create app_user pool")
		DeferCleanup(appPool.Close)
		resolver = graph.NewPGResolver(db.NewTenantPool(appPool))
		ctxA, err = tenant.WithTenant(ctx, tenantA)
		Expect(err).NotTo(HaveOccurred())
		ctxB, err = tenant.WithTenant(ctx, tenantB)
		Expect(err).NotTo(HaveOccurred())
	})

	It("serves only the job's relationship rows, not its requires rows", func() {
		data, err := resolver.Resolve(ctxA, graph.ResourceRef{URI: "pg://results/" + jobA + "/synthesis"})
		Expect(err).NotTo(HaveOccurred())
		Expect(data).To(MatchJSON(`[
			{"source_id":"c1","target_id":"c2","relationship_type":"EQUIVALENT","confidence":0.9,"viability":0.7,"is_viable":true},
			{"source_id":"c2","target_id":"c3","relationship_type":"SUBSET_OF","confidence":0.8,"viability":0,"is_viable":false}]`))
	})

	It("serves no rows when another tenant asks for the job", func() {
		data, err := resolver.Resolve(ctxB, graph.ResourceRef{URI: "pg://results/" + jobA + "/synthesis"})
		Expect(err).NotTo(HaveOccurred())
		Expect(data).To(MatchJSON(`[]`))
	})

	It("still serves the other tenant's own job, so the empty result is isolation", func() {
		data, err := resolver.Resolve(ctxB, graph.ResourceRef{URI: "pg://results/" + jobB + "/synthesis"})
		Expect(err).NotTo(HaveOccurred())
		Expect(data).To(MatchJSON(`[{"source_id":"c1","target_id":"c2","relationship_type":"EQUIVALENT","confidence":0.6,"viability":0.4,"is_viable":true}]`))
	})
})
