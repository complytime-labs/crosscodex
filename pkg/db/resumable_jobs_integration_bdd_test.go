//go:build integration

package db_test

import (
	"context"
	"database/sql"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
)

// resumable_jobs() is the one sanctioned cross-tenant read: crosscodexd
// resumes running jobs at startup before any tenant is entered. These specs
// prove app_user reaches running jobs only through it.
var _ = Describe("resumable_jobs", Ordered, func() {
	var (
		ctx                context.Context
		appConn            *sql.DB
		tenantA, tenantB   string
		runningA, runningB string
		completedA         string
	)

	BeforeAll(func() {
		ctx = context.Background()
		appConn = appUserConn()
		tenantA, tenantB = dbtest.TestID("resume-a"), dbtest.TestID("resume-b")
		setupTenant(tenantA, "Resume A")
		setupTenant(tenantB, "Resume B")
		runningA, runningB, completedA = dbtest.TestID("job-ra"), dbtest.TestID("job-rb"), dbtest.TestID("job-ca")
		for _, j := range []struct{ id, tenant, status string }{
			{runningA, tenantA, "running"},
			{runningB, tenantB, "running"},
			{completedA, tenantA, "completed"},
		} {
			Expect(suPool.Exec(ctx,
				"INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, $3, 'resume-test')",
				j.id, j.tenant, j.status)).To(Succeed())
		}
	})

	AfterAll(func() {
		if suPool != nil {
			for _, id := range []string{tenantA, tenantB} {
				stmt, err := dbtest.HardDeleteTenantSQL(id)
				Expect(err).NotTo(HaveOccurred())
				Expect(suPool.Exec(ctx, stmt)).To(Succeed())
			}
		}
	})

	It("hides jobs from app_user when no tenant is entered", func() {
		var n int
		Expect(appConn.QueryRowContext(ctx,
			"SELECT count(*) FROM jobs WHERE job_id = ANY($1)", []string{runningA, runningB, completedA}).Scan(&n)).To(Succeed())
		Expect(n).To(BeZero(), "RLS must hide every job from app_user outside a tenant")
	})

	It("returns every tenant's running jobs to app_user and no others", func() {
		rows, err := appConn.QueryContext(ctx,
			"SELECT job_id, tenant_id, status FROM public.resumable_jobs() WHERE job_id = ANY($1)",
			[]string{runningA, runningB, completedA})
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()
		got := map[string]string{}
		for rows.Next() {
			var id, tid, status string
			Expect(rows.Scan(&id, &tid, &status)).To(Succeed())
			Expect(status).To(Equal("running"), id)
			got[id] = tid
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		Expect(got).To(Equal(map[string]string{runningA: tenantA, runningB: tenantB}))
	})

	It("is a definer function with a pinned search_path that PUBLIC cannot execute", func() {
		var definer, publicExec bool
		var config string
		Expect(suPool.QueryRow(ctx, `
			SELECT prosecdef, coalesce(array_to_string(proconfig, ';'), ''),
			       has_function_privilege('public', oid, 'EXECUTE')
			FROM pg_proc WHERE oid = 'public.resumable_jobs()'::regprocedure`).
			Scan(&definer, &config, &publicExec)).To(Succeed())
		Expect(definer).To(BeTrue())
		Expect(config).To(Equal("search_path=pg_catalog, public, pg_temp"))
		Expect(publicExec).To(BeFalse(), "PUBLIC must not hold EXECUTE on resumable_jobs()")
	})
})
