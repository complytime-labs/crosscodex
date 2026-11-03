//go:build integration

package pgstore_test

import (
	"context"
	"fmt"
	"os"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/resultcache/pgstore"
	"github.com/complytime-labs/crosscodex/pkg/resultcache/resultcachetest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// appPool is the restricted app_user pool every spec reads and writes
// through; suPool (superuser) only seeds parents, tears tenants down and
// plays the tired admin.
var appPool, suPool db.Pool

var _ = BeforeSuite(func() {
	suDSN := os.Getenv("TEST_DATABASE_DSN")
	if suDSN == "" {
		Fail("TEST_DATABASE_DSN not set: run task test:integration:resultcache")
	}
	ctx := context.Background()
	migrator, err := db.NewMigrator(suDSN)
	Expect(err).NotTo(HaveOccurred(), "create migrator")
	Expect(migrator.Up(ctx)).To(Succeed(), "run migrations")
	Expect(migrator.Close()).To(Succeed())

	password, err := dbtest.RolePassword(dbtest.AppUserPasswordEnv)
	Expect(err).NotTo(HaveOccurred())
	Expect(dbtest.SetRolePassword(ctx, suDSN, "app_user", password)).To(Succeed())
	appDSN, err := dbtest.RoleDSN(suDSN, "app_user", password)
	Expect(err).NotTo(HaveOccurred())
	appPool, err = db.NewPool(db.PoolConfig{DSN: appDSN, MaxOpenConns: 4})
	Expect(err).NotTo(HaveOccurred())
	suPool, err = db.NewPool(db.PoolConfig{DSN: suDSN, MaxOpenConns: 2})
	Expect(err).NotTo(HaveOccurred())
})

var _ = AfterSuite(func() {
	if appPool != nil {
		Expect(appPool.Close()).To(Succeed())
	}
	if suPool != nil {
		Expect(suPool.Close()).To(Succeed())
	}
})

// ensureTenant provisions id (a privileged operation, see db.EnsureTenant)
// and removes it with every row it owns when the spec ends.
func ensureTenant(id string) {
	Expect(db.EnsureTenant(context.Background(), suPool, id, id)).To(Succeed())
	DeferCleanup(func() {
		stmt, err := dbtest.HardDeleteTenantSQL(id)
		Expect(err).NotTo(HaveOccurred())
		Expect(suPool.Exec(context.Background(), stmt)).To(Succeed())
	})
}

func seedJob(tenantID, jobID string) {
	Expect(suPool.Exec(context.Background(),
		`INSERT INTO jobs (job_id, tenant_id, status, created_by) VALUES ($1, $2, 'running', 'rc-integ') ON CONFLICT DO NOTHING`,
		jobID, tenantID)).To(Succeed())
}

func seedCatalog(tenantID, catalogID string) {
	Expect(suPool.Exec(context.Background(),
		`INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path)
		 VALUES ($1, $2, 'C', 'v1', 'test', 'unused') ON CONFLICT DO NOTHING`, catalogID, tenantID)).To(Succeed())
}

func newAppStore() *pgstore.Store {
	s, err := pgstore.New(db.NewTenantPool(appPool))
	Expect(err).NotTo(HaveOccurred())
	return s
}

var _ = Describe("pgstore against PostgreSQL as app_user", func() {
	resultcachetest.StoreContract(func(string) resultcachetest.StoreEnv {
		seen := map[string]bool{}
		provision := func(id string) {
			if !seen[id] {
				seen[id] = true
				ensureTenant(id)
			}
		}
		return resultcachetest.StoreEnv{
			Store:      newAppStore(),
			AddJob:     func(t, j string) { provision(t); seedJob(t, j) },
			AddCatalog: func(t, c string) { provision(t); seedCatalog(t, c) },
			SetJobStatus: func(t, j, status string) {
				Expect(suPool.Exec(context.Background(), `UPDATE jobs SET status = $1 WHERE job_id = $2 AND tenant_id = $3`,
					status, j, t)).To(Succeed())
			},
		}
	})
})

var _ = Describe("Result cache tables guard against the tired admin", func() {
	const (
		tenantA = "rc-guard-a"
		tenantB = "rc-guard-b"
		jobP    = "rc-guard-job-p"
		jobOfB  = "rc-guard-job-b"
		cat     = "rc-guard-cat"
		// attempt is the producer's signed stage run.
		attempt = "5d4c3b2a-1f0e-4d9c-8b7a-6f5e4d3c2b1a"
	)
	var (
		ctxA  context.Context
		store *pgstore.Store
		key   = strings.Repeat("c", 64)
		// signed is the checkpoint BeforeEach signs over the producer's
		// attempt, as the executor would when its stage stops.
		signed map[resultcache.AttemptRef]string
	)
	producerAttempt := resultcache.AttemptRef{JobID: jobP, Analyzer: "classify", Attempt: attempt}
	// signedSummary is the checkpoint as VerifiedSummaries takes it.
	signedSummary := func() map[string]resultcache.SignedAttempt {
		return map[string]resultcache.SignedAttempt{"classify": {Attempt: attempt, Digest: signed[producerAttempt]}}
	}
	// verifySigned verifies key's hit in one snapshot against signed.
	verifySigned := func() error {
		snap, err := store.LookupVerifiable(ctxA, "classify", []string{key}, []resultcache.AttemptRef{producerAttempt})
		Expect(err).NotTo(HaveOccurred())
		_, err = snap.Verify(key, signed)
		return err
	}
	// count reads a single count as app_user in tenantA's scope.
	count := func(query string, args ...any) int {
		tx, err := db.NewTenantPool(appPool).Begin(ctxA)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = tx.Rollback() }()
		var n int
		Expect(tx.QueryRow(ctxA, query, args...).Scan(&n)).To(Succeed())
		return n
	}
	// asAppUser runs stmt as app_user in tenantA's scope and returns its error.
	asAppUser := func(stmt string, args ...any) error {
		tx, err := db.NewTenantPool(appPool).Begin(ctxA)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = tx.Rollback() }()
		if err := tx.Exec(ctxA, stmt, args...); err != nil {
			return err
		}
		return tx.Commit()
	}
	// auditRecordIntact asserts the producer's write entry and stats row are
	// as BeforeEach recorded them.
	auditRecordIntact := func() {
		Expect(count(`SELECT count(*) FROM job_cache_entries WHERE job_id = $1 AND role = 'write' AND task_id = 't1'`, jobP)).To(Equal(1))
		Expect(count(`SELECT count(*) FROM job_cache_stats WHERE job_id = $1 AND tasks = 1 AND dispatched = 1`, jobP)).To(Equal(1))
	}

	BeforeEach(func() {
		ensureTenant(tenantA)
		ensureTenant(tenantB)
		seedJob(tenantA, jobP)
		seedJob(tenantB, jobOfB)
		seedCatalog(tenantA, cat)
		var err error
		ctxA, err = tenant.WithTenant(context.Background(), tenantA)
		Expect(err).NotTo(HaveOccurred())
		store = newAppStore()
		Expect(store.Put(ctxA, resultcache.Entry{JobID: jobP, Analyzer: "classify", Attempt: attempt, TaskID: "t1", CacheKey: key,
			Result: []byte(`{"model":"m","response":"x"}`), ResponseModel: "m", SourceCatalogID: cat, TargetCatalogID: cat})).To(Succeed())
		Expect(store.RecordStage(ctxA, resultcache.StageRecord{JobID: jobP, Analyzer: "classify", Attempt: attempt,
			Stats: resultcache.Stats{Mode: resultcache.ModeEnabled, Tasks: 1, Dispatched: 1}})).To(Succeed())
		record, err := store.Manifest(ctxA, jobP, "classify", attempt)
		Expect(err).NotTo(HaveOccurred())
		Expect(store.Checkpoint(ctxA, record, func(_ context.Context, digest string) error {
			signed = map[resultcache.AttemptRef]string{producerAttempt: digest}
			return nil
		})).To(Succeed())
		Expect(verifySigned()).To(Succeed())
	})

	DescribeTable("refuses app_user's changes to the audit record and the cache's protections, changing nothing",
		func(stmt string) {
			Expect(asAppUser(stmt)).To(MatchError(Or(
				ContainSubstring("permission denied"), ContainSubstring("must be owner"))))
			auditRecordIntact()
			Expect(count(`SELECT count(*) FROM llm_result_cache WHERE cache_key = $1`, key)).To(Equal(1))
		},
		Entry("UPDATE job_cache_entries", `UPDATE job_cache_entries SET role = 'hit', producer_job_id = 'x'`),
		Entry("DELETE job_cache_entries", `DELETE FROM job_cache_entries`),
		Entry("UPDATE job_cache_stats", `UPDATE job_cache_stats SET tasks = 0, dispatched = 0`),
		Entry("DELETE job_cache_stats", `DELETE FROM job_cache_stats`),
		Entry("DELETE llm_result_cache", `DELETE FROM llm_result_cache`),
		Entry("TRUNCATE job_cache_entries", `TRUNCATE job_cache_entries`),
		Entry("TRUNCATE job_cache_stats", `TRUNCATE job_cache_stats`),
		Entry("TRUNCATE llm_result_cache", `TRUNCATE llm_result_cache`),
		Entry("disable the entries triggers", `ALTER TABLE job_cache_entries DISABLE TRIGGER ALL`),
		Entry("disable the stats triggers", `ALTER TABLE job_cache_stats DISABLE TRIGGER ALL`),
		Entry("disable RLS on the entries", `ALTER TABLE job_cache_entries DISABLE ROW LEVEL SECURITY`),
		Entry("disable RLS on the stats", `ALTER TABLE job_cache_stats DISABLE ROW LEVEL SECURITY`),
		Entry("disable RLS on the cache", `ALTER TABLE llm_result_cache DISABLE ROW LEVEL SECURITY`),
	)

	// The triggers, not only the grants, keep the record append-only: a role
	// someone granted UPDATE and DELETE to, outside retention_purge, is still
	// refused, with a message naming the alternative.
	DescribeTable("refuses a granted role outside retention_purge with an actionable message, changing nothing",
		func(stmt string) {
			ctx := context.Background()
			tx, err := suPool.Begin(ctx)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = tx.Rollback() }()
			Expect(tx.Exec(ctx, `CREATE ROLE rc_tired_admin NOLOGIN`)).To(Succeed())
			Expect(tx.Exec(ctx, `GRANT USAGE ON SCHEMA public TO rc_tired_admin`)).To(Succeed())
			Expect(tx.Exec(ctx, `GRANT SELECT, UPDATE, DELETE ON job_cache_entries, job_cache_stats TO rc_tired_admin`)).To(Succeed())
			Expect(tx.Exec(ctx, `SELECT set_config('app.current_tenant', $1, true)`, tenantA)).To(Succeed())
			Expect(tx.Exec(ctx, `SET LOCAL ROLE rc_tired_admin`)).To(Succeed())
			err = tx.Exec(ctx, stmt, jobP)
			Expect(err).To(MatchError(And(
				ContainSubstring("job "+jobP),
				ContainSubstring("append-only because the job's attestation pins it"),
				ContainSubstring("start a new job"))))
			Expect(tx.Rollback()).To(Succeed())
			auditRecordIntact()
		},
		Entry("UPDATE job_cache_entries", `UPDATE job_cache_entries SET task_id = 't2' WHERE job_id = $1`),
		Entry("DELETE job_cache_entries", `DELETE FROM job_cache_entries WHERE job_id = $1`),
		Entry("UPDATE job_cache_stats", `UPDATE job_cache_stats SET tasks = 5 WHERE job_id = $1`),
		Entry("DELETE job_cache_stats", `DELETE FROM job_cache_stats WHERE job_id = $1`),
	)

	DescribeTable("refuses TRUNCATE of the audit tables even to the superuser, changing nothing",
		func(table string) {
			err := suPool.Exec(context.Background(), "TRUNCATE "+table)
			Expect(err).To(MatchError(And(
				ContainSubstring("append-only because job attestations pin it"),
				ContainSubstring("Retention purges the rows of each expired job"))))
			auditRecordIntact()
		},
		Entry("job_cache_entries", "job_cache_entries"),
		Entry("job_cache_stats", "job_cache_stats"),
	)

	It("refuses moving a cached result to another tenant, keeping it", func() {
		err := asAppUser(`UPDATE llm_result_cache SET tenant_id = $1 WHERE cache_key = $2`, tenantB, key)
		Expect(err).To(MatchError(ContainSubstring(`new row violates row-level security policy for table "llm_result_cache"`)))
		Expect(count(`SELECT count(*) FROM llm_result_cache WHERE cache_key = $1 AND tenant_id = $2`, key, tenantA)).To(Equal(1))
	})

	It("refuses re-pointing a cached result at another tenant's job, keeping it", func() {
		err := asAppUser(`UPDATE llm_result_cache SET job_id = $1 WHERE cache_key = $2`, jobOfB, key)
		Expect(err).To(MatchError(ContainSubstring("llm_result_cache_job_tenant_fkey")))
		Expect(count(`SELECT count(*) FROM llm_result_cache WHERE cache_key = $1 AND job_id = $2`, key, jobP)).To(Equal(1))
	})

	It("refuses an entry written under another tenant's ID, writing nothing", func() {
		err := asAppUser(`INSERT INTO job_cache_entries (tenant_id, job_id, analyzer, attempt, task_id, cache_key, result_digest, role)
			VALUES ($1, $2, 'classify', $3, 't9', $4, $5, 'write')`, tenantB, jobOfB, attempt, key, "sha256:"+strings.Repeat("0", 64))
		// The freeze trigger reads jobs under the caller's RLS, so another
		// tenant's job is invisible and refused before the RLS check would.
		Expect(err).To(MatchError(ContainSubstring(fmt.Sprintf(
			"cannot add a job_cache_entries row for job %s: no such job is visible in this tenant", jobOfB))))
		Expect(db.IsPgErrorCode(err, "23503")).To(BeTrue(), "an invisible job is an unknown parent (foreign_key_violation), not a frozen one")
		Expect(count(`SELECT count(*) FROM job_cache_entries WHERE task_id = 't9'`)).To(BeZero())
		bCtx, err := tenant.WithTenant(context.Background(), tenantB)
		Expect(err).NotTo(HaveOccurred())
		m, err := store.Manifest(bCtx, jobOfB, "classify", attempt)
		Expect(err).NotTo(HaveOccurred())
		Expect(m.Entries).To(BeEmpty())
	})

	It("matches zero rows without a tenant scope (fail closed)", func() {
		ctx := context.Background()
		tx, err := appPool.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = tx.Rollback() }()
		for _, table := range []string{"llm_result_cache", "job_cache_entries", "job_cache_stats"} {
			var n int
			Expect(tx.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n)).To(Succeed())
			Expect(n).To(BeZero(), table)
		}
	})

	It("detects a hand-edited cached result: the hit no longer verifies", func() {
		Expect(suPool.Exec(context.Background(), `UPDATE llm_result_cache SET result = '{"model":"m","response":"edited"}' WHERE cache_key = $1 AND tenant_id = $2`,
			key, tenantA)).To(Succeed())
		Expect(verifySigned()).To(MatchError(resultcache.ErrCorrupt))
	})

	// ADV-3: while the producer runs, the freeze cannot stop app_user from
	// adding rows to it. A write planted under any attempt but the signed one
	// is never trusted for the signed attempt, so the edited result fails.
	It("does not trust a write planted in a running job under another attempt", func() {
		forged := []byte(`{"model":"m","response":"forged"}`)
		Expect(asAppUser(`UPDATE llm_result_cache SET result = $1 WHERE cache_key = $2`, string(forged), key)).To(Succeed())
		Expect(asAppUser(`INSERT INTO job_cache_entries (tenant_id, job_id, analyzer, attempt, task_id, cache_key, result_digest, role)
			VALUES ($1, $2, 'classify', 'made-up-attempt', 't1', $3, $4, 'write')`, tenantA, jobP, key, resultcache.ResultDigest(forged))).To(Succeed())
		m, err := store.Manifest(ctxA, jobP, "classify", attempt)
		Expect(err).NotTo(HaveOccurred())
		Expect(m.Entries).To(ConsistOf(HaveField("ResultDigest", resultcache.ResultDigest([]byte(`{"model":"m","response":"x"}`)))),
			"the signed attempt's manifest does not include the planted row")
		Expect(verifySigned()).To(MatchError(resultcache.ErrCorrupt))
	})

	// ADV-7: Checkpoint's FOR UPDATE conflicts with the freeze trigger's
	// FOR SHARE, so an insert into the job waits for the seal, and the
	// record sealed cannot be missing a row that commits later.
	It("holds an insert into the job until the checkpoint's seal returns, and seals the record without it", func() {
		record, err := store.Manifest(ctxA, jobP, "classify", attempt)
		Expect(err).NotTo(HaveOccurred())
		inSeal, release := make(chan struct{}), make(chan struct{})
		var sealed string
		checkpointed := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			checkpointed <- store.Checkpoint(ctxA, record, func(_ context.Context, digest string) error {
				sealed = digest
				close(inSeal)
				<-release
				return nil
			})
		}()
		Eventually(inSeal).Should(BeClosed())
		inserted := make(chan error, 1)
		go func() {
			inserted <- store.Put(ctxA, resultcache.Entry{JobID: jobP, Analyzer: "classify", Attempt: attempt, TaskID: "t9",
				CacheKey: strings.Repeat("e", 64), Result: []byte(`{"model":"m"}`), ResponseModel: "m", SourceCatalogID: cat, TargetCatalogID: cat})
		}()
		Consistently(inserted, "750ms").ShouldNot(Receive(), "the insert waits on the job row lock")
		close(release)
		Eventually(checkpointed).Should(Receive(BeNil()))
		Eventually(inserted).Should(Receive(BeNil()), "the job still runs, so the insert lands once the lock is released")

		Expect(sealed).To(Equal(record.Digest()), "the sealed record excludes the waiting insert")
		after, err := store.Manifest(ctxA, jobP, "classify", attempt)
		Expect(err).NotTo(HaveOccurred())
		Expect(after.Entries).To(HaveLen(2))
		Expect(after.Digest()).NotTo(Equal(sealed), "so the checkpoint no longer verifies")
		Expect(verifySigned()).To(MatchError(resultcache.ErrCorrupt))
	})

	// ADV-10: the lookup takes no lock, so a producer job row another
	// transaction holds (here, an uncommitted status update) neither blocks
	// nor fails it.
	It("reads a snapshot while another transaction updates the producer's job", func() {
		tx, err := db.NewTenantPool(appPool).Begin(ctxA)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = tx.Rollback() }()
		Expect(tx.Exec(ctxA, `UPDATE jobs SET updated_at = now() WHERE job_id = $1`, jobP)).To(Succeed())
		done := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			done <- verifySigned()
		}()
		Eventually(done, "5s").Should(Receive(BeNil()))
		Expect(tx.Commit()).To(Succeed())
		Expect(verifySigned()).To(Succeed())
	})

	It("lets a running job record its cache use", func() {
		m, err := store.Manifest(ctxA, jobP, "classify", attempt)
		Expect(err).NotTo(HaveOccurred())
		Expect(m.Entries).To(HaveLen(1))
		Expect(m.Stats).To(HaveLen(1))
	})

	Context("once the producer has finished", func() {
		// finish moves jobP to status the way the executor does (app_user,
		// tenant scope), and returns jobP's manifest and summaries as they are
		// afterwards, for comparison after the refused write.
		finish := func(status string) (resultcache.Manifest, []resultcache.Summary) {
			Expect(asAppUser(`UPDATE jobs SET status = $1 WHERE job_id = $2`, status, jobP)).To(Succeed())
			m, err := store.Manifest(ctxA, jobP, "classify", attempt)
			Expect(err).NotTo(HaveOccurred())
			sums, err := store.VerifiedSummaries(ctxA, jobP, signedSummary())
			Expect(err).NotTo(HaveOccurred())
			return m, sums
		}
		unchanged := func(m resultcache.Manifest, sums []resultcache.Summary) {
			got, err := store.Manifest(ctxA, jobP, "classify", attempt)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(m))
			gotSums, err := store.VerifiedSummaries(ctxA, jobP, signedSummary())
			Expect(err).NotTo(HaveOccurred())
			Expect(gotSums).To(Equal(sums))
		}
		frozen := func(table, status string) types.GomegaMatcher {
			return MatchError(And(
				ContainSubstring(fmt.Sprintf(`cannot add a %s row for job %s: its status is %q`, table, jobP, status)),
				ContainSubstring("only while the job runs"),
				ContainSubstring("start a new job")))
		}

		DescribeTable("refuses a write entry for it, changing nothing",
			func(status string) {
				m, sums := finish(status)
				err := store.Put(ctxA, resultcache.Entry{JobID: jobP, Analyzer: "classify", Attempt: attempt, TaskID: "t2", CacheKey: strings.Repeat("d", 64),
					Result: []byte(`{"model":"m"}`), ResponseModel: "m", SourceCatalogID: cat, TargetCatalogID: cat})
				Expect(err).To(frozen("job_cache_entries", status))
				unchanged(m, sums)
				Expect(count(`SELECT count(*) FROM llm_result_cache WHERE cache_key = $1`, strings.Repeat("d", 64))).To(BeZero(),
					"the cache row is rolled back with its write entry")
			},
			Entry("completed", "completed"),
			Entry("failed", "failed"),
			Entry("cancelled", "cancelled"),
		)

		DescribeTable("refuses a stats row for it, changing nothing",
			func(status string) {
				m, sums := finish(status)
				err := store.RecordStage(ctxA, resultcache.StageRecord{JobID: jobP, Analyzer: "classify", Attempt: attempt,
					Stats: resultcache.Stats{Mode: resultcache.ModeEnabled, Tasks: 9, Dispatched: 9}})
				Expect(err).To(frozen("job_cache_stats", status))
				unchanged(m, sums)
			},
			Entry("completed", "completed"),
			Entry("failed", "failed"),
			Entry("cancelled", "cancelled"),
		)

		// The forgery the freeze exists for: edit the cached result, then
		// record a write digest matching the edit under the producer, so the
		// edited result would verify.
		It("refuses a forged write digest for an edited result, which still fails verification", func() {
			m, sums := finish("completed")
			forged := []byte(`{"model":"m","response":"forged"}`)
			Expect(asAppUser(`UPDATE llm_result_cache SET result = $1 WHERE cache_key = $2`, string(forged), key)).To(Succeed())
			err := asAppUser(`INSERT INTO job_cache_entries (tenant_id, job_id, analyzer, attempt, task_id, cache_key, result_digest, role)
				VALUES ($1, $2, 'classify', $3, 't1', $4, $5, 'write')`, tenantA, jobP, attempt, key, resultcache.ResultDigest(forged))
			Expect(err).To(frozen("job_cache_entries", "completed"))
			unchanged(m, sums)
			Expect(verifySigned()).To(MatchError(resultcache.ErrCorrupt))
		})

		// The trigger runs as app_user, which may create temporary tables;
		// its pinned search_path and schema-qualified read keep a temporary
		// "jobs" from standing in for the real one.
		It("is not fooled by a temporary jobs table claiming the job runs", func() {
			m, sums := finish("completed")
			tx, err := db.NewTenantPool(appPool).Begin(ctxA)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = tx.Rollback() }()
			Expect(tx.Exec(ctxA, `CREATE TEMP TABLE jobs (job_id text, tenant_id text, status text)`)).To(Succeed())
			Expect(tx.Exec(ctxA, `INSERT INTO pg_temp.jobs VALUES ($1, $2, 'running')`, jobP, tenantA)).To(Succeed())
			err = tx.Exec(ctxA, `INSERT INTO job_cache_stats (tenant_id, job_id, analyzer, attempt, mode, tasks, hits, dispatched)
				VALUES ($1, $2, 'classify', $3, 'enabled', 9, 0, 9)`, tenantA, jobP, attempt)
			Expect(err).To(frozen("job_cache_stats", "completed"))
			Expect(tx.Rollback()).To(Succeed())
			unchanged(m, sums)
		})
	})
})
