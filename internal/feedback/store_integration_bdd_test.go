//go:build integration

package feedback_test

import (
	"context"
	"database/sql"
	"os"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/feedback"
	analyzerresults "github.com/complytime-labs/crosscodex/pkg/analyzer/results"
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
		Fail("TEST_DATABASE_DSN not set — run: task test:integration:feedback")
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
		Expect(db.EnsureTenant(ctx, suPool, id, "feedback store it")).To(Succeed())
	}
	DeferCleanup(func() {
		for _, id := range ids {
			stmt, err := dbtest.HardDeleteTenantSQL(id)
			Expect(err).NotTo(HaveOccurred())
			Expect(suPool.Exec(ctx, stmt)).To(Succeed())
		}
	})
}

// seedReviewer registers reviewerID in tenantID through the superuser, as
// 'crosscodexd admin reviewer add' (and 'delete') would.
func seedReviewer(tenantID, reviewerID, status string) {
	ctx := context.Background()
	Expect(suPool.Exec(ctx, `INSERT INTO reviewers (tenant_id, reviewer_id, created_by) VALUES ($1, $2, 'test')`,
		tenantID, reviewerID)).To(Succeed())
	if status == reviewerDeleted {
		Expect(suPool.Exec(ctx, `UPDATE reviewers SET status = 'deleted', deleted_at = now(), deleted_by = 'test'
			WHERE tenant_id = $1 AND reviewer_id = $2`, tenantID, reviewerID)).To(Succeed())
	}
}

// seedJob stores j and its mappings as the relationship analyzer's
// vote_summaries rows, through the superuser, as a pipeline run would.
func seedJob(tenantID string, j jobSeed) {
	ctx := context.Background()
	Expect(suPool.Exec(ctx, `INSERT INTO jobs (job_id, tenant_id, status, config, created_by) VALUES ($1, $2, $3, '{}'::jsonb, $4)`,
		j.ID, tenantID, j.Status, j.CreatedBy)).To(Succeed())
	for _, m := range j.Mappings {
		Expect(suPool.Exec(ctx, `INSERT INTO vote_summaries
			(job_id, source_id, target_id, analyzer_name, consensus, confidence, viability, tenant_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			j.ID, m.SourceID, m.TargetID, analyzerresults.RelationshipAnalyzer, m.RelationshipType, m.Confidence, m.Viability,
			tenantID)).To(Succeed())
	}
}

// The Postgres store must pass the same contract fakeStore does, as the
// restricted app_user role under row-level security. Tenants, reviewers and
// jobs are created through the superuser, the privileged path.
var _ = Describe("PGStore contract", func() {
	storeContract(func(token string) storeEnv {
		a, b := contractTenants(token)
		createTenants(a, b)
		return storeEnv{
			Store:        feedback.NewPGStore(db.NewTenantPool(appPool)),
			SeedReviewer: seedReviewer,
			SeedJob:      seedJob,
			Determinations: func(tenantID string) []feedback.Determination {
				rows, err := suPool.Query(context.Background(), `SELECT determination_id, mapping_id, job_id, source_id, target_id,
					outcome, decided_by, rationale, request, attestation, decided_at
					FROM mapping_determinations WHERE tenant_id = $1`, tenantID)
				Expect(err).NotTo(HaveOccurred())
				defer rows.Close() //nolint:errcheck
				var out []feedback.Determination
				for rows.Next() {
					var (
						d       feedback.Determination
						outcome string
					)
					Expect(rows.Scan(&d.ID, &d.MappingID, &d.JobID, &d.SourceID, &d.TargetID, &outcome, &d.DecidedBy,
						&d.Rationale, &d.Request, &d.Attestation, &d.DecidedAt)).To(Succeed())
					d.Outcome, d.DecidedAt = feedback.ReviewStatus(outcome), d.DecidedAt.UTC()
					out = append(out, d)
				}
				Expect(rows.Err()).NotTo(HaveOccurred())
				return out
			},
		}
	})
})

var _ = Describe("PGStore against Postgres", func() {
	var (
		tenantA, tenantB, jobID string
		ctxA                    context.Context
		store                   *feedback.PGStore
		d                       feedback.Determination
	)

	BeforeEach(func() {
		tenantA, tenantB = "feedback-it-a-"+uuid.NewString(), "feedback-it-b-"+uuid.NewString()
		createTenants(tenantA, tenantB)
		seedReviewer(tenantA, "alice", reviewerActive)
		jobID = "feedback-it-" + uuid.NewString()
		seedJob(tenantA, jobSeed{ID: jobID, Status: "completed", CreatedBy: "analyst", Mappings: []feedback.Mapping{
			{SourceID: "s1", TargetID: "t1", RelationshipType: "EQUIVALENT_TO", Confidence: 0.9, Viability: 1},
			{SourceID: "s1", TargetID: "t2", RelationshipType: "SUBSET_OF", Confidence: 0.4, Viability: 1},
			{SourceID: "s2", TargetID: "t3", RelationshipType: "SUBSET_OF", Confidence: 0.4, Viability: 1},
			{SourceID: "s3", TargetID: "t4", RelationshipType: "SUBSET_OF", Confidence: 0.4, Viability: 1},
		}})
		var err error
		ctxA, err = tenant.WithTenant(context.Background(), tenantA)
		Expect(err).NotTo(HaveOccurred())
		store = feedback.NewPGStore(db.NewTenantPool(appPool))

		Expect(store.RecordVote(ctxA, tenantA, feedback.Vote{ID: "v-1", MappingID: feedback.MappingID(jobID, "s1", "t1"),
			JobID: jobID, SourceID: "s1", TargetID: "t1", Voter: "alice", Type: feedback.VoteDisagree,
			Rationale: "scopes differ", CreatedAt: contractTime})).To(Succeed())
		d = feedback.Determination{ID: "d-1", MappingID: feedback.MappingID(jobID, "s1", "t1"), JobID: jobID,
			SourceID: "s1", TargetID: "t1", Outcome: feedback.ReviewInvalid, DecidedBy: "alice", Rationale: "scopes differ",
			Request: []byte("{}"), Attestation: []byte("sig"), DecidedAt: contractTime.Add(time.Minute)}
		Expect(store.Determine(ctxA, tenantA, d, "", feedback.Review{MappingID: d.MappingID, JobID: jobID, SourceID: "s1",
			TargetID: "t1", Status: d.Outcome, DeterminationID: d.ID, DecidedAt: d.DecidedAt, EdgeIDs: []string{"e-1"}})).To(Succeed())
	})

	// inTenant runs fn in an app_user transaction scoped to tenantID, then
	// rolls it back.
	inTenant := func(tenantID string, fn func(ctx context.Context, tx *sql.Tx)) {
		ctx := context.Background()
		tx, err := appDB.BeginTx(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(ctx, "SELECT public.enter_tenant($1)", tenantID)
		Expect(err).NotTo(HaveOccurred())
		fn(ctx, tx)
	}
	asApp := func(tenantID, stmt string) error {
		var err error
		inTenant(tenantID, func(ctx context.Context, tx *sql.Tx) { _, err = tx.ExecContext(ctx, stmt) })
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

	It("hides every feedback row and the cleaning state view from another tenant's app_user session", func() {
		inTenant(tenantB, func(ctx context.Context, tx *sql.Tx) {
			for _, table := range []string{"mapping_votes", "mapping_determinations", "mapping_reviews", "mapping_cleaning_states"} {
				var n int
				Expect(tx.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n)).To(Succeed())
				Expect(n).To(BeZero(), table)
			}
		})
	})

	DescribeTable("refuses app_user changes the role is not granted with 42501 and leaves the rows unchanged",
		func(table, stmt string) {
			before := tableJSON(table)
			err := asApp(tenantA, stmt)
			Expect(db.IsPgErrorCode(err, "42501")).To(BeTrue(), "got %v", err)
			Expect(tableJSON(table)).To(Equal(before))
		},
		Entry("UPDATE of a vote", "mapping_votes", `UPDATE mapping_votes SET voter = 'mallory'`),
		Entry("DELETE of a vote", "mapping_votes", `DELETE FROM mapping_votes`),
		Entry("TRUNCATE of votes", "mapping_votes", `TRUNCATE mapping_votes`),
		Entry("UPDATE of a determination", "mapping_determinations", `UPDATE mapping_determinations SET decided_by = 'mallory'`),
		Entry("DELETE of a determination", "mapping_determinations", `DELETE FROM mapping_determinations`),
		Entry("TRUNCATE of determinations", "mapping_determinations", `TRUNCATE mapping_determinations CASCADE`),
		Entry("DELETE of a review", "mapping_reviews", `DELETE FROM mapping_reviews`),
		Entry("TRUNCATE of reviews", "mapping_reviews", `TRUNCATE mapping_reviews`),
	)

	DescribeTable("refuses even the owner's changes to votes and determinations, saying why and what to do",
		func(table, stmt, code, why string) {
			votes, determinations := tableJSON("mapping_votes"), tableJSON("mapping_determinations")
			err := suPool.Exec(context.Background(), stmt)
			Expect(db.IsPgErrorCode(err, code)).To(BeTrue(), "got %v", err)
			Expect(err).To(MatchError(ContainSubstring(why)))
			Expect(tableJSON("mapping_votes")).To(Equal(votes))
			Expect(tableJSON("mapping_determinations")).To(Equal(determinations))
		},
		Entry("UPDATE of a vote", "mapping_votes", `UPDATE mapping_votes SET voter = 'mallory'`,
			"42501", "append-only record of who voted what"),
		Entry("DELETE of a vote", "mapping_votes", `DELETE FROM mapping_votes`,
			"42501", "To change your view of a mapping record a new vote"),
		Entry("TRUNCATE of votes", "mapping_votes", `TRUNCATE mapping_votes`,
			"42501", "so TRUNCATE is refused"),
		Entry("UPDATE of a determination", "mapping_determinations", `UPDATE mapping_determinations SET outcome = 'valid'`,
			"42501", "append-only record of who decided what"),
		Entry("DELETE of a determination", "mapping_determinations", `DELETE FROM mapping_determinations`,
			"42501", "To reverse a determination record a new one"),
		// Reviews reference determinations, so Postgres refuses a plain
		// TRUNCATE before the refusal trigger runs, and CASCADE reaches it.
		Entry("plain TRUNCATE of determinations, by the foreign key", "mapping_determinations", `TRUNCATE mapping_determinations`,
			"0A000", "referenced in a foreign key constraint"),
		Entry("TRUNCATE of determinations with CASCADE, by the append-only trigger", "mapping_determinations",
			`TRUNCATE mapping_determinations CASCADE`, "42501", "so TRUNCATE is refused"),
	)

	DescribeTable("refuses a determination whose stale outcome and decider disagree, even written directly",
		func(outcome, decidedBy string) {
			before := tableJSON("mapping_determinations")
			err := asApp(tenantA, `INSERT INTO mapping_determinations
				(tenant_id, determination_id, mapping_id, job_id, source_id, target_id, outcome, decided_by, rationale,
				 request, attestation, decided_at)
				VALUES (current_setting('app.current_tenant'), 'd-direct', 'm', 'j', 's', 't', '`+outcome+`', '`+decidedBy+`',
				        'r', '\x00', '\x00', now())`)
			Expect(db.IsPgErrorCode(err, "23514")).To(BeTrue(), "got %v", err)
			Expect(err).To(MatchError(ContainSubstring("mapping_determinations_reproject")))
			Expect(tableJSON("mapping_determinations")).To(Equal(before))
		},
		Entry("stale by a reviewer", string(feedback.ReviewStale), "alice"),
		Entry("invalid by re-projection", string(feedback.ReviewInvalid), feedback.DecidedByReproject),
	)

	Describe("mapping_cleaning_states", func() {
		type stateRow struct{ job, source, target, state, origin, ref string }
		rowsOf := func(tenantID string) []stateRow {
			var out []stateRow
			inTenant(tenantID, func(ctx context.Context, tx *sql.Tx) {
				rows, err := tx.QueryContext(ctx, `SELECT job_id, source_id, target_id, state, source, ref_id
					FROM mapping_cleaning_states ORDER BY source, job_id, source_id, target_id`)
				Expect(err).NotTo(HaveOccurred())
				defer func() { _ = rows.Close() }()
				for rows.Next() {
					var r stateRow
					Expect(rows.Scan(&r.job, &r.source, &r.target, &r.state, &r.origin, &r.ref)).To(Succeed())
					out = append(out, r)
				}
				Expect(rows.Err()).NotTo(HaveOccurred())
			})
			return out
		}

		// determine records a determination on the job's mapping of src to
		// tgt quoting prev, with the given edges, and marks it projected.
		determine := func(id, src, tgt, prev string, outcome feedback.ReviewStatus, at time.Duration, edges ...string) feedback.Determination {
			dd := feedback.Determination{ID: id, MappingID: feedback.MappingID(jobID, src, tgt), JobID: jobID,
				SourceID: src, TargetID: tgt, Outcome: outcome, DecidedBy: "alice", Rationale: "checked",
				Request: []byte("{}"), Attestation: []byte("sig"), DecidedAt: contractTime.Add(at)}
			Expect(store.Determine(ctxA, tenantA, dd, prev, feedback.Review{MappingID: dd.MappingID, JobID: jobID,
				SourceID: src, TargetID: tgt, Status: outcome, DeterminationID: id, DecidedAt: dd.DecidedAt, EdgeIDs: edges})).To(Succeed())
			Expect(store.MarkProjected(ctxA, tenantA, dd.MappingID, id, edges)).To(Succeed())
			return dd
		}

		It("shows a stale review as stale and a valid review that restored edges as restored", func() {
			retired := determine("d-s1", "s2", "t3", "", feedback.ReviewInvalid, time.Hour, "e-s")
			stale := feedback.Determination{ID: "d-s2", MappingID: retired.MappingID, JobID: jobID, SourceID: "s2",
				TargetID: "t3", Outcome: feedback.ReviewStale, DecidedBy: feedback.DecidedByReproject, Rationale: "edges gone",
				Request: []byte("{}"), Attestation: []byte("sig"), DecidedAt: contractTime.Add(2 * time.Hour)}
			Expect(store.MarkStale(ctxA, tenantA, stale, retired.ID)).To(Succeed())
			determine("d-r1", "s3", "t4", "", feedback.ReviewInvalid, time.Hour, "e-r")
			restored := determine("d-r2", "s3", "t4", "d-r1", feedback.ReviewValid, 2*time.Hour, "e-r2")

			Expect(rowsOf(tenantA)).To(Equal([]stateRow{
				{jobID, "s1", "t1", "retired", "determination", d.ID},
				{jobID, "s2", "t3", "stale", "determination", stale.ID},
				{jobID, "s3", "t4", "restored", "determination", restored.ID},
			}))
			Expect(rowsOf(tenantB)).To(BeEmpty())
		})

		It("shows a retired finding and an invalid review as retired, and a valid review without edge changes not at all", func() {
			f := cleaningtest.ContractFinding(1, cleaning.SignalConflictsWith, cleaning.StatusRetired, "cat-x", "cat-y")
			run := cleaningtest.ContractRun(1)
			Expect(cleaning.NewPGFindingStore(db.NewTenantPool(appPool)).ApplyRun(ctxA, tenantA, cleaning.RunWrite{
				Run:     run,
				Upserts: cleaningtest.NewUpserts(f),
				Decisions: []cleaning.Decision{cleaningtest.PolicyDecision(cleaning.Decision{ID: "c-1", FindingID: f.ID,
					Verdict: cleaning.VerdictRetire, Rationale: "policy", Request: []byte("{}"), Attestation: []byte("sig")}, run)},
			})).To(Succeed())
			valid := feedback.Determination{ID: "d-2", MappingID: feedback.MappingID(jobID, "s1", "t2"), JobID: jobID,
				SourceID: "s1", TargetID: "t2", Outcome: feedback.ReviewValid, DecidedBy: "alice", Rationale: "same scope",
				Request: []byte("{}"), Attestation: []byte("sig"), DecidedAt: contractTime.Add(time.Hour)}
			Expect(store.Determine(ctxA, tenantA, valid, "", feedback.Review{MappingID: valid.MappingID, JobID: jobID,
				SourceID: "s1", TargetID: "t2", Status: valid.Outcome, DeterminationID: valid.ID, DecidedAt: valid.DecidedAt})).To(Succeed())

			Expect(rowsOf(tenantA)).To(Equal([]stateRow{
				{jobID, "s1", "t1", "retired", "determination", d.ID},
				{"job-1", "s1", "t1", "retired", "finding", f.ID},
			}))
			Expect(rowsOf(tenantB)).To(BeEmpty())
		})
	})
})
