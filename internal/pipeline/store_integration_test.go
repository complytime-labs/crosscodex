//go:build integration

package pipeline_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/google/uuid"
)

// assertJSONEqual compares two JSON payloads for semantic equality,
// tolerating the whitespace normalization Postgres's JSONB type applies on
// write (e.g. `{"a":1}` is read back as `{"a": 1}`). Offset 1 so a failure
// here is reported at the caller's line.
func assertJSONEqual(label string, got, want []byte) {
	var gotVal, wantVal any
	ExpectWithOffset(1, json.Unmarshal(got, &gotVal)).To(Succeed(), "%s: unmarshaling got JSON failed", label)
	ExpectWithOffset(1, json.Unmarshal(want, &wantVal)).To(Succeed(), "%s: unmarshaling want JSON failed", label)
	ExpectWithOffset(1, gotVal).To(Equal(wantVal), "%s result_data: got %s, want %s", label, got, want)
}

// appUserDSN swaps userinfo to app_user and this run's app_user password
func appUserDSN(suDSN string) (string, error) {
	u, err := url.Parse(suDSN)
	if err != nil {
		return "", err
	}
	pw, err := dbtest.RolePassword(dbtest.AppUserPasswordEnv)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword("app_user", pw)
	return u.String(), nil
}

// setupPGStore runs migrations, ensures the app_user role, builds superuser and
// app pools, provisions a unique tenant, and returns a tenant-scoped context.
// Pools are closed via DeferCleanup. Skips when TEST_DATABASE_DSN is unset.
func setupPGStore() (*pipeline.PGStore, db.Pool, string, context.Context) {
	ctx := context.Background()

	suDSN := os.Getenv("TEST_DATABASE_DSN")
	if suDSN == "" {
		Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
	}

	migrator, err := db.NewMigrator(suDSN)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to create migrator")
	ExpectWithOffset(1, migrator.Up(ctx)).To(Succeed(), "failed to run migrations")
	ExpectWithOffset(1, migrator.Close()).To(Succeed(), "failed to close migrator")

	adminDB, err := sql.Open("pgx", suDSN)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to open admin connection")
	appPassword, err := dbtest.RolePassword(dbtest.AppUserPasswordEnv)
	Expect(err).NotTo(HaveOccurred(), "app_user password")
	stmt, err := dbtest.AlterRolePasswordSQL("app_user", appPassword)
	Expect(err).NotTo(HaveOccurred(), "build ALTER ROLE app_user")
	_, err = adminDB.ExecContext(ctx, stmt)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to set app_user password")
	ExpectWithOffset(1, adminDB.Close()).To(Succeed(), "failed to close admin connection")

	suPool, err := db.NewPool(db.PoolConfig{
		DSN:          suDSN,
		MaxOpenConns: 5,
		Extensions:   []string{"age", "vector"},
	})
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to create superuser pool")
	DeferCleanup(func() { suPool.Close() })

	appDSN, err := appUserDSN(suDSN)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to build app_user DSN")
	appPool, err := db.NewPool(db.PoolConfig{
		DSN:          appDSN,
		MaxOpenConns: 2,
	})
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to create app_user pool")
	DeferCleanup(func() { appPool.Close() })

	tenantConn := db.NewTenantPool(appPool)

	tenantID := fmt.Sprintf("pipeline-it-%s", uuid.New().String())
	ExpectWithOffset(1, db.EnsureTenant(ctx, suPool, tenantID, "pipeline-it")).To(Succeed(), "failed to ensure tenant")

	ctx, err = tenant.WithTenant(ctx, tenantID)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to set tenant context")

	return pipeline.NewPGStore(tenantConn, suPool), suPool, tenantID, ctx
}

// newITJob builds a valid pending Job for the given tenant.
func newITJob(tenantID string) *pipeline.Job {
	now := time.Now().UTC()
	return &pipeline.Job{
		JobID:     uuid.New().String(),
		TenantID:  tenantID,
		Status:    pipeline.JobStatusPending,
		Config:    []byte(`{}`),
		CreatedBy: "integration-test",
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// stagesByName indexes stages by StageName for assertion.
func stagesByName(stages []*pipeline.Stage) map[string]*pipeline.Stage {
	m := make(map[string]*pipeline.Stage, len(stages))
	for _, s := range stages {
		m[s.StageName] = s
	}
	return m
}

var _ = Describe("PGStore integration", func() {
	It("creates a job and round-trips it via GetJob", func() {
		ctx := context.Background()

		suDSN := os.Getenv("TEST_DATABASE_DSN")
		if suDSN == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		migrator, err := db.NewMigrator(suDSN)
		Expect(err).NotTo(HaveOccurred(), "failed to create migrator")
		Expect(migrator.Up(ctx)).To(Succeed(), "failed to run migrations")
		Expect(migrator.Close()).To(Succeed(), "failed to close migrator")

		adminDB, err := sql.Open("pgx", suDSN)
		Expect(err).NotTo(HaveOccurred(), "failed to open admin connection")
		appPassword, err := dbtest.RolePassword(dbtest.AppUserPasswordEnv)
		Expect(err).NotTo(HaveOccurred(), "app_user password")
		stmt, err := dbtest.AlterRolePasswordSQL("app_user", appPassword)
		Expect(err).NotTo(HaveOccurred(), "build ALTER ROLE app_user")
		_, err = adminDB.ExecContext(ctx, stmt)
		Expect(err).NotTo(HaveOccurred(), "failed to set app_user password")
		Expect(adminDB.Close()).To(Succeed(), "failed to close admin connection")

		suPool, err := db.NewPool(db.PoolConfig{
			DSN:          suDSN,
			MaxOpenConns: 5,
			Extensions:   []string{"age", "vector"},
		})
		Expect(err).NotTo(HaveOccurred(), "failed to create superuser pool")
		defer suPool.Close()

		appDSN, err := appUserDSN(suDSN)
		Expect(err).NotTo(HaveOccurred(), "failed to build app_user DSN")
		appPool, err := db.NewPool(db.PoolConfig{
			DSN:          appDSN,
			MaxOpenConns: 2,
		})
		Expect(err).NotTo(HaveOccurred(), "failed to create app_user pool")
		defer appPool.Close()

		tenantConn := db.NewTenantPool(appPool)

		tenantID := fmt.Sprintf("pipeline-it-%s", uuid.New().String())
		Expect(db.EnsureTenant(ctx, suPool, tenantID, "pipeline-it")).To(Succeed(), "failed to ensure tenant")

		ctx, err = tenant.WithTenant(ctx, tenantID)
		Expect(err).NotTo(HaveOccurred(), "failed to set tenant context")

		store := pipeline.NewPGStore(tenantConn, suPool)

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

		Expect(store.CreateJob(ctx, job)).To(Succeed(), "CreateJob failed")

		got, err := store.GetJob(ctx, job.JobID)
		Expect(err).NotTo(HaveOccurred(), "GetJob failed")
		Expect(got.JobID).To(Equal(job.JobID), "JobID mismatch")
		Expect(got.TenantID).To(Equal(job.TenantID), "TenantID mismatch")
		Expect(got.Status).To(Equal(job.Status), "Status mismatch")
	})

	It("CompleteAnalysisStage upserts analysis_results and completes the stage", func() {
		ctx := context.Background()

		suDSN := os.Getenv("TEST_DATABASE_DSN")
		if suDSN == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		migrator, err := db.NewMigrator(suDSN)
		Expect(err).NotTo(HaveOccurred(), "failed to create migrator")
		Expect(migrator.Up(ctx)).To(Succeed(), "failed to run migrations")
		Expect(migrator.Close()).To(Succeed(), "failed to close migrator")

		adminDB, err := sql.Open("pgx", suDSN)
		Expect(err).NotTo(HaveOccurred(), "failed to open admin connection")
		appPassword, err := dbtest.RolePassword(dbtest.AppUserPasswordEnv)
		Expect(err).NotTo(HaveOccurred(), "app_user password")
		stmt, err := dbtest.AlterRolePasswordSQL("app_user", appPassword)
		Expect(err).NotTo(HaveOccurred(), "build ALTER ROLE app_user")
		_, err = adminDB.ExecContext(ctx, stmt)
		Expect(err).NotTo(HaveOccurred(), "failed to set app_user password")
		Expect(adminDB.Close()).To(Succeed(), "failed to close admin connection")

		suPool, err := db.NewPool(db.PoolConfig{
			DSN:          suDSN,
			MaxOpenConns: 5,
			Extensions:   []string{"age", "vector"},
		})
		Expect(err).NotTo(HaveOccurred(), "failed to create superuser pool")
		defer suPool.Close()

		appDSN, err := appUserDSN(suDSN)
		Expect(err).NotTo(HaveOccurred(), "failed to build app_user DSN")
		appPool, err := db.NewPool(db.PoolConfig{
			DSN:          appDSN,
			MaxOpenConns: 2,
		})
		Expect(err).NotTo(HaveOccurred(), "failed to create app_user pool")
		defer appPool.Close()

		tenantConn := db.NewTenantPool(appPool)

		tenantID := fmt.Sprintf("pipeline-it-%s", uuid.New().String())
		Expect(db.EnsureTenant(ctx, suPool, tenantID, "pipeline-it")).To(Succeed(), "failed to ensure tenant")

		ctx, err = tenant.WithTenant(ctx, tenantID)
		Expect(err).NotTo(HaveOccurred(), "failed to set tenant context")

		store := pipeline.NewPGStore(tenantConn, suPool)

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
		Expect(store.CreateJob(ctx, job)).To(Succeed(), "CreateJob failed")

		stageNames := []string{"requires"}
		Expect(store.CreateStages(ctx, job.JobID, stageNames)).To(Succeed(), "CreateStages failed")

		// First write: upsert an initial result.
		first := []byte(`[{"source_id":"AC-1","target_id":"AC-2","confidence":0.9}]`)
		Expect(store.CompleteAnalysisStage(ctx, job.JobID, "requires", first)).To(Succeed(), "CompleteAnalysisStage (first write) failed")

		// Second write to the same (tenant, job, analyzer) key: must upsert, not duplicate.
		second := []byte(`[{"source_id":"AC-1","target_id":"AC-3","confidence":0.7}]`)
		Expect(store.CompleteAnalysisStage(ctx, job.JobID, "requires", second)).To(Succeed(), "CompleteAnalysisStage (second write) failed")

		// Verify the durably committed row directly. analysis_results has RLS
		// (policy tenant_isolation gates on app.current_tenant), so a raw app_user
		// read outside a tenant-scoped transaction sees nothing. Read as the
		// superuser, which bypasses RLS, to assert the row physically committed
		// with the expected content. The app-role RLS read path is separately
		// exercised by store.GetStages below.
		//
		// Exactly one row survives, and it reflects the second write's content.
		var count int
		row := suPool.QueryRow(ctx,
			"SELECT count(*) FROM analysis_results WHERE tenant_id = $1 AND job_id = $2 AND analyzer_name = $3",
			tenantID, job.JobID, "requires")
		Expect(row.Scan(&count)).To(Succeed(), "counting analysis_results rows failed")
		Expect(count).To(Equal(1), "analysis_results row count (upsert should not duplicate)")

		var data []byte
		row = suPool.QueryRow(ctx,
			"SELECT result_data FROM analysis_results WHERE tenant_id = $1 AND job_id = $2 AND analyzer_name = $3",
			tenantID, job.JobID, "requires")
		Expect(row.Scan(&data)).To(Succeed(), "reading result_data failed")

		var gotResult, wantResult []map[string]any
		Expect(json.Unmarshal(data, &gotResult)).To(Succeed(), "unmarshaling stored result_data failed")
		Expect(json.Unmarshal(second, &wantResult)).To(Succeed(), "unmarshaling expected result_data failed")
		Expect(gotResult).To(Equal(wantResult), "result_data should reflect the second write")

		// The job_stages row must be completed.
		stages, err := store.GetStages(ctx, job.JobID)
		Expect(err).NotTo(HaveOccurred(), "GetStages failed")
		Expect(stages).To(HaveLen(1))
		Expect(stages[0].Status).To(Equal(pipeline.StageStatusCompleted), "stage status")
		Expect(stages[0].CompletedAt).NotTo(BeNil(), "stage CompletedAt")
	})

	It("GetCompletedAnalysisResults returns only completed analyzers, scoped per tenant", func() {
		ctx := context.Background()

		suDSN := os.Getenv("TEST_DATABASE_DSN")
		if suDSN == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		migrator, err := db.NewMigrator(suDSN)
		Expect(err).NotTo(HaveOccurred(), "failed to create migrator")
		Expect(migrator.Up(ctx)).To(Succeed(), "failed to run migrations")
		Expect(migrator.Close()).To(Succeed(), "failed to close migrator")

		adminDB, err := sql.Open("pgx", suDSN)
		Expect(err).NotTo(HaveOccurred(), "failed to open admin connection")
		appPassword, err := dbtest.RolePassword(dbtest.AppUserPasswordEnv)
		Expect(err).NotTo(HaveOccurred(), "app_user password")
		stmt, err := dbtest.AlterRolePasswordSQL("app_user", appPassword)
		Expect(err).NotTo(HaveOccurred(), "build ALTER ROLE app_user")
		_, err = adminDB.ExecContext(ctx, stmt)
		Expect(err).NotTo(HaveOccurred(), "failed to set app_user password")
		Expect(adminDB.Close()).To(Succeed(), "failed to close admin connection")

		suPool, err := db.NewPool(db.PoolConfig{
			DSN:          suDSN,
			MaxOpenConns: 5,
			Extensions:   []string{"age", "vector"},
		})
		Expect(err).NotTo(HaveOccurred(), "failed to create superuser pool")
		defer suPool.Close()

		appDSN, err := appUserDSN(suDSN)
		Expect(err).NotTo(HaveOccurred(), "failed to build app_user DSN")
		appPool, err := db.NewPool(db.PoolConfig{
			DSN:          appDSN,
			MaxOpenConns: 2,
		})
		Expect(err).NotTo(HaveOccurred(), "failed to create app_user pool")
		defer appPool.Close()

		tenantConn := db.NewTenantPool(appPool)

		tenantID := fmt.Sprintf("pipeline-it-%s", uuid.New().String())
		Expect(db.EnsureTenant(ctx, suPool, tenantID, "pipeline-it")).To(Succeed(), "failed to ensure tenant")

		ctx, err = tenant.WithTenant(ctx, tenantID)
		Expect(err).NotTo(HaveOccurred(), "failed to set tenant context")

		store := pipeline.NewPGStore(tenantConn, suPool)

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
		Expect(store.CreateJob(ctx, job)).To(Succeed(), "CreateJob failed")

		analyzerNames := []string{"classify", "embedding", "requires"}
		Expect(store.CreateStages(ctx, job.JobID, analyzerNames)).To(Succeed(), "CreateStages failed")

		// Complete "classify" and "embedding" with distinct payloads; leave
		// "requires" never completed.
		classifyData := []byte(`{"a":1}`)
		Expect(store.CompleteAnalysisStage(ctx, job.JobID, "classify", classifyData)).To(Succeed(), "CompleteAnalysisStage(classify) failed")
		embeddingData := []byte(`{"b":2}`)
		Expect(store.CompleteAnalysisStage(ctx, job.JobID, "embedding", embeddingData)).To(Succeed(), "CompleteAnalysisStage(embedding) failed")

		// Superset query: includes the never-completed "requires" analyzer.
		// Only the two completed analyzers should come back.
		results, err := store.GetCompletedAnalysisResults(ctx, job.JobID, []string{"classify", "embedding", "requires"})
		Expect(err).NotTo(HaveOccurred(), "GetCompletedAnalysisResults (superset) failed")
		Expect(results).To(HaveLen(2), "want 2 entries (results=%+v)", results)
		_, ok := results["requires"]
		Expect(ok).To(BeFalse(), `"requires" present, want absent (never completed)`)

		classifyResult, ok := results["classify"]
		Expect(ok).To(BeTrue(), `"classify" missing from results`)
		Expect(classifyResult.AnalyzerName).To(Equal("classify"))
		// result_data is stored as JSONB, which normalizes whitespace on write
		// (e.g. `{"a":1}` becomes `{"a": 1}`), so compare parsed values rather
		// than raw bytes — mirrors the CompleteAnalysisStage spec's approach.
		assertJSONEqual("classify", classifyResult.ResultData, classifyData)

		embeddingResult, ok := results["embedding"]
		Expect(ok).To(BeTrue(), `"embedding" missing from results`)
		Expect(embeddingResult.AnalyzerName).To(Equal("embedding"))
		assertJSONEqual("embedding", embeddingResult.ResultData, embeddingData)

		// Subset query: only "classify" requested.
		subsetResults, err := store.GetCompletedAnalysisResults(ctx, job.JobID, []string{"classify"})
		Expect(err).NotTo(HaveOccurred(), "GetCompletedAnalysisResults (subset) failed")
		Expect(subsetResults).To(HaveLen(1), "results=%+v", subsetResults)
		_, ok = subsetResults["classify"]
		Expect(ok).To(BeTrue(), `"classify" missing from results`)

		// Tenant isolation: a second tenant's job and completed analyzer must
		// never surface under the first tenant's context, even when the query
		// deliberately targets the second tenant's job ID and analyzer name.
		tenantID2 := fmt.Sprintf("pipeline-it-%s", uuid.New().String())
		Expect(db.EnsureTenant(ctx, suPool, tenantID2, "pipeline-it-2")).To(Succeed(), "failed to ensure second tenant")
		ctx2, err := tenant.WithTenant(context.Background(), tenantID2)
		Expect(err).NotTo(HaveOccurred(), "failed to set second tenant context")

		job2 := &pipeline.Job{
			JobID:     uuid.New().String(),
			TenantID:  tenantID2,
			Status:    pipeline.JobStatusPending,
			Config:    []byte(`{}`),
			CreatedBy: "integration-test",
			CreatedAt: now,
			UpdatedAt: now,
		}
		Expect(store.CreateJob(ctx2, job2)).To(Succeed(), "CreateJob (tenant 2) failed")
		Expect(store.CreateStages(ctx2, job2.JobID, []string{"classify"})).To(Succeed(), "CreateStages (tenant 2) failed")
		tenant2Data := []byte(`{"c":3}`)
		Expect(store.CompleteAnalysisStage(ctx2, job2.JobID, "classify", tenant2Data)).To(Succeed(), "CompleteAnalysisStage (tenant 2) failed")

		// Query under tenant 1's context, but with tenant 2's job ID and
		// analyzer name. Tenant isolation must prevent any row from surfacing.
		isolationResults, err := store.GetCompletedAnalysisResults(ctx, job2.JobID, []string{"classify"})
		Expect(err).NotTo(HaveOccurred(), "GetCompletedAnalysisResults (cross-tenant) failed")
		Expect(isolationResults).To(BeEmpty(), "leaked tenant 2 data: %+v", isolationResults)
	})

	It("WriteVoteSummaries writes rows once and leaves them unchanged on retry", func() {
		ctx := context.Background()

		suDSN := os.Getenv("TEST_DATABASE_DSN")
		if suDSN == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		migrator, err := db.NewMigrator(suDSN)
		Expect(err).NotTo(HaveOccurred(), "failed to create migrator")
		Expect(migrator.Up(ctx)).To(Succeed(), "failed to run migrations")
		Expect(migrator.Close()).To(Succeed(), "failed to close migrator")

		adminDB, err := sql.Open("pgx", suDSN)
		Expect(err).NotTo(HaveOccurred(), "failed to open admin connection")
		appPassword, err := dbtest.RolePassword(dbtest.AppUserPasswordEnv)
		Expect(err).NotTo(HaveOccurred(), "app_user password")
		stmt, err := dbtest.AlterRolePasswordSQL("app_user", appPassword)
		Expect(err).NotTo(HaveOccurred(), "build ALTER ROLE app_user")
		_, err = adminDB.ExecContext(ctx, stmt)
		Expect(err).NotTo(HaveOccurred(), "failed to set app_user password")
		Expect(adminDB.Close()).To(Succeed(), "failed to close admin connection")

		suPool, err := db.NewPool(db.PoolConfig{
			DSN:          suDSN,
			MaxOpenConns: 5,
			Extensions:   []string{"age", "vector"},
		})
		Expect(err).NotTo(HaveOccurred(), "failed to create superuser pool")
		defer suPool.Close()

		appDSN, err := appUserDSN(suDSN)
		Expect(err).NotTo(HaveOccurred(), "failed to build app_user DSN")
		appPool, err := db.NewPool(db.PoolConfig{
			DSN:          appDSN,
			MaxOpenConns: 2,
		})
		Expect(err).NotTo(HaveOccurred(), "failed to create app_user pool")
		defer appPool.Close()

		tenantConn := db.NewTenantPool(appPool)

		tenantID := fmt.Sprintf("pipeline-it-%s", uuid.New().String())
		Expect(db.EnsureTenant(ctx, suPool, tenantID, "pipeline-it")).To(Succeed(), "failed to ensure tenant")

		ctx, err = tenant.WithTenant(ctx, tenantID)
		Expect(err).NotTo(HaveOccurred(), "failed to set tenant context")

		store := pipeline.NewPGStore(tenantConn, suPool)

		// vote_summaries.job_id has a foreign key to jobs(job_id), so a job must
		// exist before writing vote_summaries rows.
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
		Expect(store.CreateJob(ctx, job)).To(Succeed(), "CreateJob failed")

		pairs := []pipeline.VoteSummaryPair{
			{SourceID: "AC-1", TargetID: "AC-2", Consensus: "requires", Confidence: 0.9},
			{SourceID: "AC-3", TargetID: "AC-4", Consensus: "supports", Confidence: 0.8},
		}

		// First write: both rows must land with viability=0.
		Expect(store.WriteVoteSummaries(ctx, tenantID, job.JobID, pairs)).To(Succeed(), "WriteVoteSummaries (first write) failed")

		// vote_summaries has RLS (policy tenant_isolation gates on
		// app.current_tenant); read as the superuser, which bypasses RLS, to
		// assert the rows physically committed with the expected content —
		// mirrors the CompleteAnalysisStage spec's readback approach.
		type row struct {
			sourceID, targetID, consensus string
			confidence, viability         float64
		}
		readRows := func() []row {
			rows, err := suPool.Query(ctx,
				"SELECT source_id, target_id, consensus, confidence, viability FROM vote_summaries WHERE tenant_id = $1 AND job_id = $2 ORDER BY source_id",
				tenantID, job.JobID)
			ExpectWithOffset(1, err).NotTo(HaveOccurred(), "querying vote_summaries failed")
			defer rows.Close()

			var got []row
			for rows.Next() {
				var r row
				ExpectWithOffset(1, rows.Scan(&r.sourceID, &r.targetID, &r.consensus, &r.confidence, &r.viability)).To(Succeed(), "scanning vote_summaries row failed")
				got = append(got, r)
			}
			ExpectWithOffset(1, rows.Err()).NotTo(HaveOccurred(), "iterating vote_summaries rows failed")
			return got
		}

		got := readRows()
		Expect(got).To(HaveLen(2), "vote_summaries row count after first write (rows=%+v)", got)
		want := []row{
			{sourceID: "AC-1", targetID: "AC-2", consensus: "requires", confidence: 0.9, viability: 0},
			{sourceID: "AC-3", targetID: "AC-4", consensus: "supports", confidence: 0.8, viability: 0},
		}
		Expect(got).To(Equal(want), "vote_summaries rows after first write")

		// Second write with the same pairs: ON CONFLICT DO NOTHING must leave the
		// existing rows untouched (no error, no duplicates) — this is what makes
		// a resumed job's re-run of the same requires/relationship pass safe
		// against vote_summaries' immutability-on-UPDATE trigger.
		Expect(store.WriteVoteSummaries(ctx, tenantID, job.JobID, pairs)).To(Succeed(), "WriteVoteSummaries (second write, same pairs) failed")

		gotAfterRetry := readRows()
		Expect(gotAfterRetry).To(HaveLen(2), "vote_summaries row count after second write (no duplicates; rows=%+v)", gotAfterRetry)
		Expect(gotAfterRetry).To(Equal(want), "vote_summaries rows after second write must be unchanged")
	})

	It("ResetStagesFrom resets the failed stage and all later stages, leaving earlier stages untouched", func() {
		store, _, tenantID, ctx := setupPGStore()

		job := newITJob(tenantID)
		Expect(store.CreateJob(ctx, job)).To(Succeed(), "CreateJob failed")
		stageNames := []string{"classify", "requires", "synthesis", "graph"}
		Expect(store.CreateStages(ctx, job.JobID, stageNames)).To(Succeed(), "CreateStages failed")

		// classify completes; requires fails.
		Expect(store.UpdateStageStatus(ctx, job.JobID, "classify", pipeline.StageStatusCompleted)).To(Succeed(), "UpdateStageStatus(classify, completed) failed")
		Expect(store.UpdateStageError(ctx, job.JobID, "requires", errors.New("boom"))).To(Succeed(), "UpdateStageError failed")

		stages, err := store.GetStages(ctx, job.JobID)
		Expect(err).NotTo(HaveOccurred(), "GetStages after UpdateStageError failed")
		byName := stagesByName(stages)
		Expect(byName["requires"].Status).To(Equal(pipeline.StageStatusFailed), "requires status")
		Expect(byName["requires"].ErrorMessage).To(Equal("boom"), "requires error_message")

		// Reset from the failed stage onward.
		Expect(store.ResetStagesFrom(ctx, job.JobID, "requires", stageNames)).To(Succeed(), "ResetStagesFrom failed")

		stages, err = store.GetStages(ctx, job.JobID)
		Expect(err).NotTo(HaveOccurred(), "GetStages after ResetStagesFrom failed")
		byName = stagesByName(stages)

		// Earlier stage untouched.
		Expect(byName["classify"].Status).To(Equal(pipeline.StageStatusCompleted), "classify status after reset (untouched)")
		Expect(byName["classify"].RetryCount).To(Equal(0), "classify retry_count after reset (untouched)")
		// fromStage and all later stages reset to pending with incremented retry_count.
		for _, name := range []string{"requires", "synthesis", "graph"} {
			Expect(byName[name].Status).To(Equal(pipeline.StageStatusPending), "%s status after reset", name)
			Expect(byName[name].RetryCount).To(Equal(1), "%s retry_count after reset", name)
			Expect(byName[name].ErrorMessage).To(BeEmpty(), "%s error_message after reset", name)
		}
	})

	It("GetResumableJobs returns running jobs but excludes completed ones", func() {
		store, _, tenantID, ctx := setupPGStore()

		// One running job (should be returned) and one completed job (should not).
		running := newITJob(tenantID)
		running.Status = pipeline.JobStatusPending
		Expect(store.CreateJob(ctx, running)).To(Succeed(), "CreateJob(running) failed")
		Expect(store.UpdateJobStatus(ctx, running.JobID, pipeline.JobStatusRunning, nil)).To(Succeed(), "UpdateJobStatus(running) failed")

		completed := newITJob(tenantID)
		Expect(store.CreateJob(ctx, completed)).To(Succeed(), "CreateJob(completed) failed")
		Expect(store.UpdateJobStatus(ctx, completed.JobID, pipeline.JobStatusCompleted, nil)).To(Succeed(), "UpdateJobStatus(completed) failed")

		jobs, err := store.GetResumableJobs(ctx)
		Expect(err).NotTo(HaveOccurred(), "GetResumableJobs failed")

		found := map[string]bool{}
		for _, j := range jobs {
			found[j.JobID] = true
			Expect(j.Status).To(Equal(pipeline.JobStatusRunning), "GetResumableJobs returned job %s", j.JobID)
		}
		Expect(found[running.JobID]).To(BeTrue(), "GetResumableJobs missing the running job %s", running.JobID)
		Expect(found[completed.JobID]).To(BeFalse(), "GetResumableJobs returned the completed job %s, want excluded", completed.JobID)
	})

	It("ListJobs filters by status and paginates without overlap", func() {
		store, _, tenantID, ctx := setupPGStore()

		// Three completed jobs and one pending job for this tenant.
		var completedIDs []string
		for i := 0; i < 3; i++ {
			j := newITJob(tenantID)
			Expect(store.CreateJob(ctx, j)).To(Succeed(), "CreateJob failed")
			Expect(store.UpdateJobStatus(ctx, j.JobID, pipeline.JobStatusCompleted, nil)).To(Succeed(), "UpdateJobStatus(completed) failed")
			completedIDs = append(completedIDs, j.JobID)
		}
		pending := newITJob(tenantID)
		Expect(store.CreateJob(ctx, pending)).To(Succeed(), "CreateJob(pending) failed")

		// Filter by completed: total 3.
		jobs, total, err := store.ListJobs(ctx, tenantID, pipeline.JobFilter{Status: pipeline.JobStatusCompleted, Limit: 2, Offset: 0})
		Expect(err).NotTo(HaveOccurred(), "ListJobs(page 1) failed")
		Expect(total).To(Equal(int64(3)), "ListJobs total")
		Expect(jobs).To(HaveLen(2), "ListJobs page 1 len (limit)")
		for _, j := range jobs {
			Expect(j.Status).To(Equal(pipeline.JobStatusCompleted), "ListJobs returned status (filtered)")
		}

		// Second page returns the remaining completed job.
		page2, _, err := store.ListJobs(ctx, tenantID, pipeline.JobFilter{Status: pipeline.JobStatusCompleted, Limit: 2, Offset: 2})
		Expect(err).NotTo(HaveOccurred(), "ListJobs(page 2) failed")
		Expect(page2).To(HaveLen(1), "ListJobs page 2 len")

		// The two pages together cover exactly the three completed jobs we created,
		// with no overlap — proving pagination returns the right rows, not just the
		// right counts.
		returned := make(map[string]bool)
		for _, j := range jobs {
			returned[j.JobID] = true
		}
		for _, j := range page2 {
			returned[j.JobID] = true
		}
		Expect(returned).To(HaveLen(3), "ListJobs pages covered distinct jobs (no overlap)")
		for _, id := range completedIDs {
			Expect(returned[id]).To(BeTrue(), "ListJobs pages missing completed job %s", id)
		}

		// No filter: all four jobs for the tenant.
		_, totalAll, err := store.ListJobs(ctx, tenantID, pipeline.JobFilter{})
		Expect(err).NotTo(HaveOccurred(), "ListJobs(no filter) failed")
		Expect(totalAll).To(Equal(int64(4)), "ListJobs total (no filter)")
	})
})
