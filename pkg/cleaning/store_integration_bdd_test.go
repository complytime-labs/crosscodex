//go:build integration

package cleaning_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/cleaning/cleaningtest"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// Initialised by BeforeSuite, closed by AfterSuite: appPool and appDB log in
// as the restricted app_user, suPool as the superuser for setup and checks.
var (
	appPool db.Pool
	appDB   *sql.DB
	suPool  db.Pool
)

var _ = BeforeSuite(func() {
	suDSN := os.Getenv("TEST_DATABASE_DSN")
	if suDSN == "" {
		Fail("TEST_DATABASE_DSN not set — run: task test:integration:cleaning")
	}
	ctx := context.Background()

	migrator, err := db.NewMigrator(suDSN)
	Expect(err).NotTo(HaveOccurred(), "create migrator")
	Expect(migrator.Up(ctx)).To(Succeed(), "run migrations")
	Expect(migrator.Close()).To(Succeed(), "close migrator")

	appPassword, err := dbtest.RolePassword(dbtest.AppUserPasswordEnv)
	Expect(err).NotTo(HaveOccurred(), "app_user password")
	Expect(dbtest.SetRolePassword(ctx, suDSN, "app_user", appPassword)).To(Succeed(), "set app_user password")
	appDSN, err := dbtest.RoleDSN(suDSN, "app_user", appPassword)
	Expect(err).NotTo(HaveOccurred(), "app_user DSN")

	appPool, err = db.NewPool(db.PoolConfig{DSN: appDSN, MaxOpenConns: 5})
	Expect(err).NotTo(HaveOccurred(), "app_user pool")
	appDB, err = sql.Open("pgx", appDSN)
	Expect(err).NotTo(HaveOccurred(), "app_user connection")
	suPool, err = db.NewPool(db.PoolConfig{DSN: suDSN, MaxOpenConns: 5, Extensions: []string{"age", "vector"}})
	Expect(err).NotTo(HaveOccurred(), "superuser pool")
})

var _ = AfterSuite(func() {
	if appDB != nil {
		Expect(appDB.Close()).To(Succeed())
	}
	for _, p := range []db.Pool{appPool, suPool} {
		if p != nil {
			p.Close() //nolint:errcheck
		}
	}
})

// createTenants creates the tenants and removes them, with every row
// carrying their IDs, when the spec ends.
func createTenants(ids ...string) {
	ctx := context.Background()
	for _, id := range ids {
		Expect(db.EnsureTenant(ctx, suPool, id, "cleaning store it")).To(Succeed())
	}
	DeferCleanup(func() {
		for _, id := range ids {
			stmt, err := dbtest.HardDeleteTenantSQL(id)
			Expect(err).NotTo(HaveOccurred())
			Expect(suPool.Exec(ctx, stmt)).To(Succeed())
		}
	})
}

// The Postgres store must pass the same contract cleaningtest.Fake does, as
// the restricted app_user role under row-level security. Tenants and
// reviewers are created through the superuser, the privileged path.
var _ = Describe("PGFindingStore contract", func() {
	cleaningtest.FindingStoreContract(func(token string) cleaningtest.FindingStoreEnv {
		a, b := cleaningtest.ContractTenants(token)
		createTenants(a, b)
		return cleaningtest.FindingStoreEnv{
			Store: cleaning.NewPGFindingStore(db.NewTenantPool(appPool)),
			Runs: func(tenantID string) []cleaning.RunRecord {
				rows, err := suPool.Query(context.Background(), `SELECT plan_token, plan, attestation, applied_at
					FROM edge_cleaning_runs WHERE tenant_id = $1 ORDER BY applied_at, plan_token COLLATE "C"`, tenantID)
				Expect(err).NotTo(HaveOccurred())
				defer rows.Close() //nolint:errcheck
				var out []cleaning.RunRecord
				for rows.Next() {
					var r cleaning.RunRecord
					Expect(rows.Scan(&r.PlanToken, &r.Plan, &r.Attestation, &r.AppliedAt)).To(Succeed())
					r.AppliedAt = r.AppliedAt.UTC()
					out = append(out, r)
				}
				Expect(rows.Err()).NotTo(HaveOccurred())
				return out
			},
			SeedReviewer: func(tenantID, reviewerID, status string) {
				ctx := context.Background()
				Expect(suPool.Exec(ctx, `INSERT INTO reviewers (tenant_id, reviewer_id, created_by) VALUES ($1, $2, 'test')`,
					tenantID, reviewerID)).To(Succeed())
				if status == cleaningtest.ReviewerDeleted {
					Expect(suPool.Exec(ctx, `UPDATE reviewers SET status = 'deleted', deleted_at = now(), deleted_by = 'test'
						WHERE tenant_id = $1 AND reviewer_id = $2`, tenantID, reviewerID)).To(Succeed())
				}
			},
		}
	})
})

var _ = Describe("PGFindingStore against Postgres", func() {
	var (
		tenantA, tenantB string
		ctxA             context.Context
		store            *cleaning.PGFindingStore
	)

	BeforeEach(func() {
		tenantA, tenantB = "cleaning-it-a-"+uuid.NewString(), "cleaning-it-b-"+uuid.NewString()
		createTenants(tenantA, tenantB)
		var err error
		ctxA, err = tenant.WithTenant(context.Background(), tenantA)
		Expect(err).NotTo(HaveOccurred())
		store = cleaning.NewPGFindingStore(db.NewTenantPool(appPool))
	})

	// asApp runs stmt as app_user inside a transaction scoped to tenantID.
	asApp := func(tenantID, stmt string) error {
		ctx := context.Background()
		tx, err := appDB.BeginTx(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(ctx, "SELECT public.enter_tenant($1)", tenantID)
		Expect(err).NotTo(HaveOccurred())
		_, err = tx.ExecContext(ctx, stmt)
		return err
	}
	// tableJSON returns tenantA's rows of table, read as the superuser.
	tableJSON := func(table string) string {
		var s sql.NullString
		Expect(suPool.QueryRow(context.Background(),
			`SELECT jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text)::text FROM `+table+` t WHERE tenant_id = $1`,
			tenantA).Scan(&s)).To(Succeed())
		Expect(s.Valid).To(BeTrue(), "%s has rows for the tenant", table)
		return s.String
	}

	// The one spec at the real 128 MiB edge_ids bound; every other bound spec
	// lowers it through cleaning.SetStoredListLimit.
	It("refuses edge IDs over the real edge_ids bound with ErrFindingTooLarge and leaves the row unchanged", func() {
		f := cleaningtest.ContractFinding(1, cleaning.SignalStalePrompt, cleaning.StatusRetired, "cat-x", "cat-y")
		f.GraphApplied = false
		Expect(store.ApplyRun(ctxA, tenantA, cleaning.RunWrite{Run: cleaningtest.ContractRun(1), Upserts: cleaningtest.NewUpserts(f)})).To(Succeed())
		err := store.MarkProjected(ctxA, tenantA, f.ID, []string{strings.Repeat("e", 128<<20)})
		Expect(err).To(MatchError(cleaning.ErrFindingTooLarge))
		Expect(err).To(MatchError(ContainSubstring("the edge_ids column would hold 134217732 bytes of JSON, over the 134217728-byte limit")))
		Expect(err).To(MatchError(ContainSubstring("nothing was written")))
		got, _, err := store.GetFinding(ctxA, tenantA, f.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.EdgeIDs).To(Equal(f.EdgeIDs))
		Expect(got.GraphApplied).To(BeFalse())
	})

	Describe("immutability", func() {
		BeforeEach(func() {
			f := cleaningtest.ContractFinding(1, cleaning.SignalConflictsWith, cleaning.StatusRetired, "cat-x", "cat-y")
			run := cleaningtest.ContractRun(1)
			Expect(store.ApplyRun(ctxA, tenantA, cleaning.RunWrite{
				Run:     run,
				Upserts: cleaningtest.NewUpserts(f),
				Decisions: []cleaning.Decision{cleaningtest.PolicyDecision(cleaning.Decision{ID: "d-1", FindingID: f.ID,
					Verdict: cleaning.VerdictRetire, Rationale: "policy", Request: []byte("{}"), Attestation: []byte("sig")}, run)},
			})).To(Succeed())
		})

		DescribeTable("refuses app_user changes to runs and decisions with 42501 and leaves them unchanged",
			func(table, stmt string) {
				before := tableJSON(table)
				err := asApp(tenantA, stmt)
				Expect(db.IsPgErrorCode(err, "42501")).To(BeTrue(), "got %v", err)
				Expect(tableJSON(table)).To(Equal(before))
			},
			Entry("UPDATE of a decision", "edge_finding_decisions", `UPDATE edge_finding_decisions SET decided_by = 'mallory'`),
			Entry("DELETE of a decision", "edge_finding_decisions", `DELETE FROM edge_finding_decisions`),
			Entry("TRUNCATE of decisions", "edge_finding_decisions", `TRUNCATE edge_finding_decisions`),
			Entry("UPDATE of a run", "edge_cleaning_runs", `UPDATE edge_cleaning_runs SET plan = '\x00'`),
			Entry("DELETE of a run", "edge_cleaning_runs", `DELETE FROM edge_cleaning_runs`),
			Entry("TRUNCATE of runs", "edge_cleaning_runs", `TRUNCATE edge_cleaning_runs`),
		)

		DescribeTable("refuses even the owner's changes to runs and decisions with an actionable message",
			func(table, stmt, record, remedy string) {
				before := tableJSON(table)
				err := suPool.Exec(context.Background(), stmt)
				Expect(db.IsPgErrorCode(err, "42501")).To(BeTrue(), "got %v", err)
				Expect(err).To(MatchError(ContainSubstring(record)))
				Expect(err).To(MatchError(ContainSubstring(remedy)))
				Expect(tableJSON(table)).To(Equal(before))
			},
			Entry("UPDATE of a decision", "edge_finding_decisions", `UPDATE edge_finding_decisions SET decided_by = 'mallory'`,
				"append-only record of who decided what", "record a new one"),
			Entry("DELETE of a decision", "edge_finding_decisions", `DELETE FROM edge_finding_decisions`,
				"append-only record of who decided what", "record a new one"),
			Entry("TRUNCATE of decisions", "edge_finding_decisions", `TRUNCATE edge_finding_decisions`,
				"append-only record of who decided what", "record a new one"),
			Entry("UPDATE of a run", "edge_cleaning_runs", `UPDATE edge_cleaning_runs SET plan = '\x00'`,
				"append-only record of applied cleaning runs", "apply a new run"),
			Entry("DELETE of a run", "edge_cleaning_runs", `DELETE FROM edge_cleaning_runs`,
				"append-only record of applied cleaning runs", "apply a new run"),
		)

		// Decisions reference runs, so Postgres refuses a plain TRUNCATE of
		// runs before the refusal trigger runs, and CASCADE reaches the
		// triggers of both tables.
		DescribeTable("refuses even the owner's TRUNCATE of runs, saying why, and leaves runs and decisions unchanged",
			func(stmt, code, why string) {
				runs, decisions := tableJSON("edge_cleaning_runs"), tableJSON("edge_finding_decisions")
				err := suPool.Exec(context.Background(), stmt)
				Expect(db.IsPgErrorCode(err, code)).To(BeTrue(), "got %v", err)
				Expect(err).To(MatchError(ContainSubstring(why)))
				Expect(tableJSON("edge_cleaning_runs")).To(Equal(runs))
				Expect(tableJSON("edge_finding_decisions")).To(Equal(decisions))
			},
			Entry("plain, by the foreign key", `TRUNCATE edge_cleaning_runs`, "0A000", "referenced in a foreign key constraint"),
			Entry("with CASCADE, by the append-only triggers", `TRUNCATE edge_cleaning_runs CASCADE`, "42501", "append-only record"),
		)

		It("refuses a policy decision that names no recorded run, even written directly", func() {
			before := tableJSON("edge_finding_decisions")
			var finding string
			Expect(suPool.QueryRow(context.Background(), `SELECT finding_id FROM edge_findings WHERE tenant_id = $1`, tenantA).
				Scan(&finding)).To(Succeed())
			ctx := context.Background()
			tx, err := appDB.BeginTx(ctx, nil)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = tx.Rollback() }()
			_, err = tx.ExecContext(ctx, "SELECT public.enter_tenant($1)", tenantA)
			Expect(err).NotTo(HaveOccurred())
			run := cleaningtest.ContractRun(1)
			_, err = tx.ExecContext(ctx, `INSERT INTO edge_finding_decisions
				(tenant_id, decision_id, finding_id, verdict, decided_by, run_id, rationale, request, attestation, decided_at)
				VALUES ($1, 'd-orphan', $2, 'retire', 'policy', $3, 'r', '\x00', '\x00', $4)`,
				tenantA, finding, run.PlanToken, run.AppliedAt.Add(time.Second))
			Expect(db.IsPgErrorCode(err, "23503")).To(BeTrue(), "got %v", err)
			Expect(tableJSON("edge_finding_decisions")).To(Equal(before))
		})

		It("refuses app_user DELETE on findings with 42501 and leaves them unchanged", func() {
			before := tableJSON("edge_findings")
			err := asApp(tenantA, `DELETE FROM edge_findings`)
			Expect(db.IsPgErrorCode(err, "42501")).To(BeTrue(), "got %v", err)
			Expect(tableJSON("edge_findings")).To(Equal(before))
		})

		DescribeTable("refuses a decision row whose provenance is inconsistent, even written directly",
			func(decidedBy, runID string) {
				before := tableJSON("edge_finding_decisions")
				var finding string
				Expect(suPool.QueryRow(context.Background(), `SELECT finding_id FROM edge_findings WHERE tenant_id = $1`, tenantA).
					Scan(&finding)).To(Succeed())
				ctx := context.Background()
				tx, err := appDB.BeginTx(ctx, nil)
				Expect(err).NotTo(HaveOccurred())
				defer func() { _ = tx.Rollback() }()
				_, err = tx.ExecContext(ctx, "SELECT public.enter_tenant($1)", tenantA)
				Expect(err).NotTo(HaveOccurred())
				var run any
				if runID != "" {
					run = runID
				}
				_, err = tx.ExecContext(ctx, `INSERT INTO edge_finding_decisions
					(tenant_id, decision_id, finding_id, verdict, decided_by, run_id, rationale, request, attestation)
					VALUES ($1, 'd-direct', $2, 'retire', $3, $4, 'r', '\x00', '\x00')`, tenantA, finding, decidedBy, run)
				Expect(db.IsPgErrorCode(err, "23514")).To(BeTrue(), "got %v", err)
				Expect(err).To(MatchError(ContainSubstring("edge_finding_decisions_provenance")))
				Expect(tableJSON("edge_finding_decisions")).To(Equal(before))
			},
			Entry("a policy decision without a run ID", cleaning.DecidedByPolicy, ""),
			Entry("a reviewer's decision with a run ID", "alice", "run-1"),
		)

		It("hides the rows from another tenant's app_user session", func() {
			ctx := context.Background()
			tx, err := appDB.BeginTx(ctx, nil)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = tx.Rollback() }()
			_, err = tx.ExecContext(ctx, "SELECT public.enter_tenant($1)", tenantB)
			Expect(err).NotTo(HaveOccurred())
			for _, table := range []string{"edge_findings", "edge_finding_decisions", "edge_cleaning_runs"} {
				var n int
				Expect(tx.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n)).To(Succeed())
				Expect(n).To(BeZero(), table)
			}
		})
	})

	Describe("CompletedJobs", func() {
		var job1, job2, jobB string
		t1 := time.Date(2030, 1, 1, 10, 0, 0, 0, time.UTC)
		t2 := t1.Add(time.Hour)
		const config = `{"catalog_id":"cat-x","target_catalog_id":"cat-y","source_catalog":{"catalog_id":"cat-x","name":"X","version":"1"}}`
		const record = `{"prompt_name":"p","prompt_version":"3","models":{"a":"rev-1"}}`

		addJobBy := func(id, tenantID, status, cfg, createdBy string) {
			Expect(suPool.Exec(context.Background(), `INSERT INTO jobs (job_id, tenant_id, status, config, created_by)
				VALUES ($1, $2, $3, $4::jsonb, $5)`, id, tenantID, status, cfg, createdBy)).To(Succeed())
		}
		addJob := func(id, tenantID, status, cfg string) { addJobBy(id, tenantID, status, cfg, "test") }
		addStage := func(jobID, tenantID, stage string, completed time.Time, rec *string) {
			Expect(suPool.Exec(context.Background(), `INSERT INTO job_stages (job_id, tenant_id, stage_name, status, completed_at, stage_record)
				VALUES ($1, $2, $3, 'completed', $4, $5)`, jobID, tenantID, stage, completed, rec)).To(Succeed())
		}

		BeforeEach(func() {
			job1, job2, jobB = "cleaning-it-"+uuid.NewString(), "cleaning-it-"+uuid.NewString(), "cleaning-it-"+uuid.NewString()
			rec := record
			addJob(job1, tenantA, "completed", config)
			addStage(job1, tenantA, "analyze", t1, &rec)
			addStage(job1, tenantA, "synthesize", t2, nil)
			addJob(job2, tenantA, "running", config) // not completed: not returned
			addStage(job2, tenantA, "analyze", t2.Add(time.Hour), &rec)
			addJob(jobB, tenantB, "completed", config) // another tenant: not returned
			addStage(jobB, tenantB, "analyze", t1, &rec)
		})

		It("returns the tenant's completed jobs with the latest stage completion and parsed records", func() {
			jobs, err := store.CompletedJobs(ctxA, tenantA)
			Expect(err).NotTo(HaveOccurred())
			Expect(jobs).To(HaveLen(1))
			Expect(jobs[0].ID).To(Equal(job1))
			Expect(jobs[0].CompletedAt).To(Equal(t2))
			Expect(jobs[0].Catalogs.SourceID).To(Equal("cat-x"))
			Expect(jobs[0].Catalogs.TargetID).To(Equal("cat-y"))
			Expect(jobs[0].Catalogs.SourceLabel.Name).To(Equal("X"))
			Expect(jobs[0].Stages).To(Equal(map[string]*cleaning.StageRecord{
				"analyze":    {PromptName: "p", PromptVersion: "3", Models: map[string]string{"a": "rev-1"}},
				"synthesize": nil,
			}))
		})

		It("returns every user's jobs through a context that carries one user", func() {
			bobs := "cleaning-it-" + uuid.NewString()
			rec := record
			addJobBy(bobs, tenantA, "completed", config, "bob")
			addStage(bobs, tenantA, "analyze", t1, &rec)
			jobs, err := store.CompletedJobs(tenant.WithUser(ctxA, "test"), tenantA)
			Expect(err).NotTo(HaveOccurred())
			got := map[string]map[string]*cleaning.StageRecord{}
			for _, j := range jobs {
				got[j.ID] = j.Stages
			}
			Expect(got).To(HaveKey(job1))
			Expect(got).To(HaveKey(bobs))
			Expect(got[bobs]).To(HaveKey("analyze"), "the stages query must also read bob's job")
		})

		It("fails closed on a non-NULL empty stage_record, naming the job and stage", func() {
			empty := ""
			addStage(job1, tenantA, "classify", t1, &empty)
			_, err := store.CompletedJobs(ctxA, tenantA)
			Expect(err).To(MatchError(cleaning.ErrInvalidStageRecord))
			Expect(err).To(MatchError(ContainSubstring(job1)))
			Expect(err).To(MatchError(ContainSubstring("classify")))
		})

		It("fails closed on a stage_record that does not parse, naming the job and stage", func() {
			bad := `{"unknown":true}`
			addStage(job1, tenantA, "classify", t1, &bad)
			_, err := store.CompletedJobs(ctxA, tenantA)
			Expect(err).To(MatchError(cleaning.ErrInvalidStageRecord))
			Expect(err).To(MatchError(ContainSubstring(job1)))
			Expect(err).To(MatchError(ContainSubstring("classify")))
		})
	})
})
