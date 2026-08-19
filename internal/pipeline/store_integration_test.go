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
	"reflect"
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

func TestPGStoreCompleteAnalysisStage(t *testing.T) {
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

	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	stageNames := []string{"requires"}
	if err := store.CreateStages(ctx, job.JobID, stageNames); err != nil {
		t.Fatalf("CreateStages failed: %v", err)
	}

	// First write: upsert an initial result.
	first := []byte(`[{"source_id":"AC-1","target_id":"AC-2","confidence":0.9}]`)
	if err := store.CompleteAnalysisStage(ctx, job.JobID, "requires", first); err != nil {
		t.Fatalf("CompleteAnalysisStage (first write) failed: %v", err)
	}

	// Second write to the same (tenant, job, analyzer) key: must upsert, not duplicate.
	second := []byte(`[{"source_id":"AC-1","target_id":"AC-3","confidence":0.7}]`)
	if err := store.CompleteAnalysisStage(ctx, job.JobID, "requires", second); err != nil {
		t.Fatalf("CompleteAnalysisStage (second write) failed: %v", err)
	}

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
	if err := row.Scan(&count); err != nil {
		t.Fatalf("counting analysis_results rows failed: %v", err)
	}
	if count != 1 {
		t.Errorf("analysis_results row count: got %d, want 1 (upsert should not duplicate)", count)
	}

	var data []byte
	row = suPool.QueryRow(ctx,
		"SELECT result_data FROM analysis_results WHERE tenant_id = $1 AND job_id = $2 AND analyzer_name = $3",
		tenantID, job.JobID, "requires")
	if err := row.Scan(&data); err != nil {
		t.Fatalf("reading result_data failed: %v", err)
	}

	var gotResult, wantResult []map[string]any
	if err := json.Unmarshal(data, &gotResult); err != nil {
		t.Fatalf("unmarshaling stored result_data failed: %v", err)
	}
	if err := json.Unmarshal(second, &wantResult); err != nil {
		t.Fatalf("unmarshaling expected result_data failed: %v", err)
	}
	if !reflect.DeepEqual(gotResult, wantResult) {
		t.Errorf("result_data: got %s, want %s (should reflect the second write)", data, second)
	}

	// The job_stages row must be completed.
	stages, err := store.GetStages(ctx, job.JobID)
	if err != nil {
		t.Fatalf("GetStages failed: %v", err)
	}
	if len(stages) != 1 {
		t.Fatalf("GetStages: got %d stages, want 1", len(stages))
	}
	if stages[0].Status != pipeline.StageStatusCompleted {
		t.Errorf("stage status: got %q, want %q", stages[0].Status, pipeline.StageStatusCompleted)
	}
	if stages[0].CompletedAt == nil {
		t.Error("stage CompletedAt: got nil, want non-nil")
	}
}

func TestPGStoreGetCompletedAnalysisResults(t *testing.T) {
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

	// 9. Build a valid Job and its stages.
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
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	analyzerNames := []string{"classify", "embedding", "requires"}
	if err := store.CreateStages(ctx, job.JobID, analyzerNames); err != nil {
		t.Fatalf("CreateStages failed: %v", err)
	}

	// Complete "classify" and "embedding" with distinct payloads; leave
	// "requires" never completed.
	classifyData := []byte(`{"a":1}`)
	if err := store.CompleteAnalysisStage(ctx, job.JobID, "classify", classifyData); err != nil {
		t.Fatalf("CompleteAnalysisStage(classify) failed: %v", err)
	}
	embeddingData := []byte(`{"b":2}`)
	if err := store.CompleteAnalysisStage(ctx, job.JobID, "embedding", embeddingData); err != nil {
		t.Fatalf("CompleteAnalysisStage(embedding) failed: %v", err)
	}

	// Superset query: includes the never-completed "requires" analyzer.
	// Only the two completed analyzers should come back.
	results, err := store.GetCompletedAnalysisResults(ctx, job.JobID, []string{"classify", "embedding", "requires"})
	if err != nil {
		t.Fatalf("GetCompletedAnalysisResults (superset) failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("GetCompletedAnalysisResults (superset): got %d entries, want 2 (results=%+v)", len(results), results)
	}
	if _, ok := results["requires"]; ok {
		t.Error(`GetCompletedAnalysisResults (superset): "requires" present, want absent (never completed)`)
	}

	classifyResult, ok := results["classify"]
	if !ok {
		t.Fatal(`GetCompletedAnalysisResults (superset): "classify" missing from results`)
	}
	if classifyResult.AnalyzerName != "classify" {
		t.Errorf("classify result AnalyzerName: got %q, want %q", classifyResult.AnalyzerName, "classify")
	}
	// result_data is stored as JSONB, which normalizes whitespace on write
	// (e.g. `{"a":1}` becomes `{"a": 1}`), so compare parsed values rather
	// than raw bytes — mirrors TestPGStoreCompleteAnalysisStage's approach.
	assertJSONEqual(t, "classify", classifyResult.ResultData, classifyData)

	embeddingResult, ok := results["embedding"]
	if !ok {
		t.Fatal(`GetCompletedAnalysisResults (superset): "embedding" missing from results`)
	}
	if embeddingResult.AnalyzerName != "embedding" {
		t.Errorf("embedding result AnalyzerName: got %q, want %q", embeddingResult.AnalyzerName, "embedding")
	}
	assertJSONEqual(t, "embedding", embeddingResult.ResultData, embeddingData)

	// Subset query: only "classify" requested.
	subsetResults, err := store.GetCompletedAnalysisResults(ctx, job.JobID, []string{"classify"})
	if err != nil {
		t.Fatalf("GetCompletedAnalysisResults (subset) failed: %v", err)
	}
	if len(subsetResults) != 1 {
		t.Fatalf("GetCompletedAnalysisResults (subset): got %d entries, want 1 (results=%+v)", len(subsetResults), subsetResults)
	}
	if _, ok := subsetResults["classify"]; !ok {
		t.Error(`GetCompletedAnalysisResults (subset): "classify" missing from results`)
	}

	// Tenant isolation: a second tenant's job and completed analyzer must
	// never surface under the first tenant's context, even when the query
	// deliberately targets the second tenant's job ID and analyzer name.
	tenantID2 := fmt.Sprintf("pipeline-it-%s", uuid.New().String())
	if err := db.EnsureTenant(ctx, suPool, tenantID2, "pipeline-it-2"); err != nil {
		t.Fatalf("failed to ensure second tenant: %v", err)
	}
	ctx2, err := tenant.WithTenant(context.Background(), tenantID2)
	if err != nil {
		t.Fatalf("failed to set second tenant context: %v", err)
	}

	job2 := &pipeline.Job{
		JobID:     uuid.New().String(),
		TenantID:  tenantID2,
		Status:    pipeline.JobStatusPending,
		Config:    []byte(`{}`),
		CreatedBy: "integration-test",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.CreateJob(ctx2, job2); err != nil {
		t.Fatalf("CreateJob (tenant 2) failed: %v", err)
	}
	if err := store.CreateStages(ctx2, job2.JobID, []string{"classify"}); err != nil {
		t.Fatalf("CreateStages (tenant 2) failed: %v", err)
	}
	tenant2Data := []byte(`{"c":3}`)
	if err := store.CompleteAnalysisStage(ctx2, job2.JobID, "classify", tenant2Data); err != nil {
		t.Fatalf("CompleteAnalysisStage (tenant 2) failed: %v", err)
	}

	// Query under tenant 1's context, but with tenant 2's job ID and
	// analyzer name. Tenant isolation must prevent any row from surfacing.
	isolationResults, err := store.GetCompletedAnalysisResults(ctx, job2.JobID, []string{"classify"})
	if err != nil {
		t.Fatalf("GetCompletedAnalysisResults (cross-tenant) failed: %v", err)
	}
	if len(isolationResults) != 0 {
		t.Errorf("GetCompletedAnalysisResults (cross-tenant): got %d entries, want 0 (leaked tenant 2 data: %+v)", len(isolationResults), isolationResults)
	}
}

func TestPGStoreWriteVoteSummaries(t *testing.T) {
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

	// 9. Build a valid Job. vote_summaries.job_id has a foreign key to
	// jobs(job_id), so a job must exist before writing vote_summaries rows.
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
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	pairs := []pipeline.VoteSummaryPair{
		{SourceID: "AC-1", TargetID: "AC-2", Consensus: "requires", Confidence: 0.9},
		{SourceID: "AC-3", TargetID: "AC-4", Consensus: "supports", Confidence: 0.8},
	}

	// First write: both rows must land with viability=0.
	if err := store.WriteVoteSummaries(ctx, tenantID, job.JobID, pairs); err != nil {
		t.Fatalf("WriteVoteSummaries (first write) failed: %v", err)
	}

	// vote_summaries has RLS (policy tenant_isolation gates on
	// app.current_tenant); read as the superuser, which bypasses RLS, to
	// assert the rows physically committed with the expected content —
	// mirrors TestPGStoreCompleteAnalysisStage's readback approach.
	type row struct {
		sourceID, targetID, consensus string
		confidence, viability         float64
	}
	readRows := func() []row {
		t.Helper()
		rows, err := suPool.Query(ctx,
			"SELECT source_id, target_id, consensus, confidence, viability FROM vote_summaries WHERE tenant_id = $1 AND job_id = $2 ORDER BY source_id",
			tenantID, job.JobID)
		if err != nil {
			t.Fatalf("querying vote_summaries failed: %v", err)
		}
		defer rows.Close()

		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.sourceID, &r.targetID, &r.consensus, &r.confidence, &r.viability); err != nil {
				t.Fatalf("scanning vote_summaries row failed: %v", err)
			}
			got = append(got, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterating vote_summaries rows failed: %v", err)
		}
		return got
	}

	got := readRows()
	if len(got) != 2 {
		t.Fatalf("vote_summaries row count after first write: got %d, want 2 (rows=%+v)", len(got), got)
	}
	want := []row{
		{sourceID: "AC-1", targetID: "AC-2", consensus: "requires", confidence: 0.9, viability: 0},
		{sourceID: "AC-3", targetID: "AC-4", consensus: "supports", confidence: 0.8, viability: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("vote_summaries rows after first write: got %+v, want %+v", got, want)
	}

	// Second write with the same pairs: ON CONFLICT DO NOTHING must leave the
	// existing rows untouched (no error, no duplicates) — this is what makes
	// a resumed job's re-run of the same requires/relationship pass safe
	// against vote_summaries' immutability-on-UPDATE trigger.
	if err := store.WriteVoteSummaries(ctx, tenantID, job.JobID, pairs); err != nil {
		t.Fatalf("WriteVoteSummaries (second write, same pairs) failed: %v", err)
	}

	gotAfterRetry := readRows()
	if len(gotAfterRetry) != 2 {
		t.Fatalf("vote_summaries row count after second write: got %d, want 2 (no duplicates; rows=%+v)", len(gotAfterRetry), gotAfterRetry)
	}
	if !reflect.DeepEqual(gotAfterRetry, want) {
		t.Errorf("vote_summaries rows after second write: got %+v, want %+v (must be unchanged)", gotAfterRetry, want)
	}
}

// assertJSONEqual compares two JSON payloads for semantic equality,
// tolerating the whitespace normalization Postgres's JSONB type applies on
// write (e.g. `{"a":1}` is read back as `{"a": 1}`).
func assertJSONEqual(t *testing.T, label string, got, want []byte) {
	t.Helper()
	var gotVal, wantVal any
	if err := json.Unmarshal(got, &gotVal); err != nil {
		t.Fatalf("%s: unmarshaling got JSON failed: %v", label, err)
	}
	if err := json.Unmarshal(want, &wantVal); err != nil {
		t.Fatalf("%s: unmarshaling want JSON failed: %v", label, err)
	}
	if !reflect.DeepEqual(gotVal, wantVal) {
		t.Errorf("%s result_data: got %s, want %s", label, got, want)
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

// setupPGStore runs migrations, ensures the app_user role, builds superuser and
// app pools, provisions a unique tenant, and returns a tenant-scoped context.
// Pools are closed via t.Cleanup. Skips when TEST_DATABASE_DSN is unset.
func setupPGStore(t *testing.T) (*pipeline.PGStore, db.Pool, string, context.Context) {
	t.Helper()
	ctx := context.Background()

	suDSN := os.Getenv("TEST_DATABASE_DSN")
	if suDSN == "" {
		t.Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
	}

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

	adminDB, err := sql.Open("pgx", suDSN)
	if err != nil {
		t.Fatalf("failed to open admin connection: %v", err)
	}
	if _, err := adminDB.ExecContext(ctx, "ALTER ROLE app_user WITH PASSWORD 'apppass'"); err != nil {
		t.Fatalf("failed to set app_user password: %v", err)
	}
	if err := adminDB.Close(); err != nil {
		t.Fatalf("failed to close admin connection: %v", err)
	}

	suPool, err := db.NewPool(db.PoolConfig{
		DSN:          suDSN,
		MaxOpenConns: 5,
		Extensions:   []string{"age", "vector"},
	})
	if err != nil {
		t.Fatalf("failed to create superuser pool: %v", err)
	}
	t.Cleanup(func() { suPool.Close() })

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
	t.Cleanup(func() { appPool.Close() })

	tenantConn := db.NewTenantPool(appPool)

	tenantID := fmt.Sprintf("pipeline-it-%s", uuid.New().String())
	if err := db.EnsureTenant(ctx, suPool, tenantID, "pipeline-it"); err != nil {
		t.Fatalf("failed to ensure tenant: %v", err)
	}

	ctx, err = tenant.WithTenant(ctx, tenantID)
	if err != nil {
		t.Fatalf("failed to set tenant context: %v", err)
	}

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

func TestPGStoreResetStagesFrom(t *testing.T) {
	store, _, tenantID, ctx := setupPGStore(t)

	job := newITJob(tenantID)
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}
	stageNames := []string{"classify", "requires", "synthesis", "graph"}
	if err := store.CreateStages(ctx, job.JobID, stageNames); err != nil {
		t.Fatalf("CreateStages failed: %v", err)
	}

	// classify completes; requires fails.
	if err := store.UpdateStageStatus(ctx, job.JobID, "classify", pipeline.StageStatusCompleted); err != nil {
		t.Fatalf("UpdateStageStatus(classify, completed) failed: %v", err)
	}
	if err := store.UpdateStageError(ctx, job.JobID, "requires", errors.New("boom")); err != nil {
		t.Fatalf("UpdateStageError failed: %v", err)
	}

	stages, err := store.GetStages(ctx, job.JobID)
	if err != nil {
		t.Fatalf("GetStages after UpdateStageError failed: %v", err)
	}
	byName := stagesByName(stages)
	if byName["requires"].Status != pipeline.StageStatusFailed {
		t.Errorf("requires status: got %q, want failed", byName["requires"].Status)
	}
	if byName["requires"].ErrorMessage != "boom" {
		t.Errorf("requires error_message: got %q, want %q", byName["requires"].ErrorMessage, "boom")
	}

	// Reset from the failed stage onward.
	if err := store.ResetStagesFrom(ctx, job.JobID, "requires", stageNames); err != nil {
		t.Fatalf("ResetStagesFrom failed: %v", err)
	}

	stages, err = store.GetStages(ctx, job.JobID)
	if err != nil {
		t.Fatalf("GetStages after ResetStagesFrom failed: %v", err)
	}
	byName = stagesByName(stages)

	// Earlier stage untouched.
	if byName["classify"].Status != pipeline.StageStatusCompleted {
		t.Errorf("classify status after reset: got %q, want completed (untouched)", byName["classify"].Status)
	}
	if byName["classify"].RetryCount != 0 {
		t.Errorf("classify retry_count after reset: got %d, want 0 (untouched)", byName["classify"].RetryCount)
	}
	// fromStage and all later stages reset to pending with incremented retry_count.
	for _, name := range []string{"requires", "synthesis", "graph"} {
		if byName[name].Status != pipeline.StageStatusPending {
			t.Errorf("%s status after reset: got %q, want pending", name, byName[name].Status)
		}
		if byName[name].RetryCount != 1 {
			t.Errorf("%s retry_count after reset: got %d, want 1", name, byName[name].RetryCount)
		}
		if byName[name].ErrorMessage != "" {
			t.Errorf("%s error_message after reset: got %q, want empty", name, byName[name].ErrorMessage)
		}
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

func TestPGStoreGetResumableJobs(t *testing.T) {
	store, _, tenantID, ctx := setupPGStore(t)

	// One running job (should be returned) and one completed job (should not).
	running := newITJob(tenantID)
	running.Status = pipeline.JobStatusPending
	if err := store.CreateJob(ctx, running); err != nil {
		t.Fatalf("CreateJob(running) failed: %v", err)
	}
	if err := store.UpdateJobStatus(ctx, running.JobID, pipeline.JobStatusRunning, nil); err != nil {
		t.Fatalf("UpdateJobStatus(running) failed: %v", err)
	}

	completed := newITJob(tenantID)
	if err := store.CreateJob(ctx, completed); err != nil {
		t.Fatalf("CreateJob(completed) failed: %v", err)
	}
	if err := store.UpdateJobStatus(ctx, completed.JobID, pipeline.JobStatusCompleted, nil); err != nil {
		t.Fatalf("UpdateJobStatus(completed) failed: %v", err)
	}

	jobs, err := store.GetResumableJobs(ctx)
	if err != nil {
		t.Fatalf("GetResumableJobs failed: %v", err)
	}

	found := map[string]bool{}
	for _, j := range jobs {
		found[j.JobID] = true
		if j.Status != pipeline.JobStatusRunning {
			t.Errorf("GetResumableJobs returned job %s with status %q, want running", j.JobID, j.Status)
		}
	}
	if !found[running.JobID] {
		t.Errorf("GetResumableJobs missing the running job %s", running.JobID)
	}
	if found[completed.JobID] {
		t.Errorf("GetResumableJobs returned the completed job %s, want excluded", completed.JobID)
	}
}

func TestPGStoreListJobs(t *testing.T) {
	store, _, tenantID, ctx := setupPGStore(t)

	// Three completed jobs and one pending job for this tenant.
	var completedIDs []string
	for i := 0; i < 3; i++ {
		j := newITJob(tenantID)
		if err := store.CreateJob(ctx, j); err != nil {
			t.Fatalf("CreateJob failed: %v", err)
		}
		if err := store.UpdateJobStatus(ctx, j.JobID, pipeline.JobStatusCompleted, nil); err != nil {
			t.Fatalf("UpdateJobStatus(completed) failed: %v", err)
		}
		completedIDs = append(completedIDs, j.JobID)
	}
	pending := newITJob(tenantID)
	if err := store.CreateJob(ctx, pending); err != nil {
		t.Fatalf("CreateJob(pending) failed: %v", err)
	}

	// Filter by completed: total 3.
	jobs, total, err := store.ListJobs(ctx, tenantID, pipeline.JobFilter{Status: pipeline.JobStatusCompleted, Limit: 2, Offset: 0})
	if err != nil {
		t.Fatalf("ListJobs(page 1) failed: %v", err)
	}
	if total != 3 {
		t.Errorf("ListJobs total: got %d, want 3", total)
	}
	if len(jobs) != 2 {
		t.Errorf("ListJobs page 1 len: got %d, want 2 (limit)", len(jobs))
	}
	for _, j := range jobs {
		if j.Status != pipeline.JobStatusCompleted {
			t.Errorf("ListJobs returned status %q, want completed (filtered)", j.Status)
		}
	}

	// Second page returns the remaining completed job.
	page2, _, err := store.ListJobs(ctx, tenantID, pipeline.JobFilter{Status: pipeline.JobStatusCompleted, Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("ListJobs(page 2) failed: %v", err)
	}
	if len(page2) != 1 {
		t.Errorf("ListJobs page 2 len: got %d, want 1", len(page2))
	}

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
	if len(returned) != 3 {
		t.Errorf("ListJobs pages covered %d distinct jobs, want 3 (no overlap)", len(returned))
	}
	for _, id := range completedIDs {
		if !returned[id] {
			t.Errorf("ListJobs pages missing completed job %s", id)
		}
	}

	// No filter: all four jobs for the tenant.
	_, totalAll, err := store.ListJobs(ctx, tenantID, pipeline.JobFilter{})
	if err != nil {
		t.Fatalf("ListJobs(no filter) failed: %v", err)
	}
	if totalAll != 4 {
		t.Errorf("ListJobs total (no filter): got %d, want 4", totalAll)
	}
}
