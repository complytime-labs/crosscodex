//go:build integration

package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/google/uuid"
)

const embeddingDim = 2000

// vectorLiteral builds a pgvector literal "[v,v,...]" with dim entries.
func vectorLiteral(dim int, val float64) string {
	parts := make([]string, dim)
	for i := range parts {
		parts[i] = fmt.Sprintf("%g", val)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// candidateITAppUserDSN swaps userinfo to app_user:apppass, mirroring
// store_integration_test.go's appUserDSN (renamed to avoid a redeclaration
// with that file, which shares this test binary).
func candidateITAppUserDSN(suDSN string) (string, error) {
	u, err := url.Parse(suDSN)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword("app_user", "apppass")
	return u.String(), nil
}

// TestCandidateGenerationIntegration exercises the full CandidateGenerator with
// the real Postgres-backed pgControlsReader/pgEmbeddingsReader/pgCandidateWriter
// against a live db (db profile). It seeds a catalog, controls (including one
// excluded section control), a classify analysis_results blob, and embeddings,
// runs Generate, and asserts requires_candidates/relationship_candidates rows
// exist for the owning tenant while a second tenant sees none (RLS).
func TestCandidateGenerationIntegration(t *testing.T) {
	ctx := context.Background()

	suDSN := os.Getenv("TEST_DATABASE_DSN")
	if suDSN == "" {
		t.Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
	}

	// Migrate (idempotent).
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

	// Ensure app_user password (idempotent).
	adminDB, err := sql.Open("pgx", suDSN)
	if err != nil {
		t.Fatalf("failed to open admin connection: %v", err)
	}
	if _, err = adminDB.ExecContext(ctx, "ALTER ROLE app_user WITH PASSWORD 'apppass'"); err != nil {
		t.Fatalf("failed to set app_user password: %v", err)
	}
	if err := adminDB.Close(); err != nil {
		t.Fatalf("failed to close admin connection: %v", err)
	}

	// Superuser pool for RLS-bypassing seed writes.
	suPool, err := db.NewPool(db.PoolConfig{
		DSN:          suDSN,
		MaxOpenConns: 5,
		Extensions:   []string{"age", "vector"},
	})
	if err != nil {
		t.Fatalf("failed to create superuser pool: %v", err)
	}
	defer suPool.Close()

	// app_user pool -> tenant-scoped connection (RLS enforced).
	appDSN, err := candidateITAppUserDSN(suDSN)
	if err != nil {
		t.Fatalf("failed to build app_user DSN: %v", err)
	}
	appPool, err := db.NewPool(db.PoolConfig{DSN: appDSN, MaxOpenConns: 2})
	if err != nil {
		t.Fatalf("failed to create app_user pool: %v", err)
	}
	defer appPool.Close()
	tenantConn := db.NewTenantPool(appPool)

	// Two tenants: the owner and an unrelated tenant used for the RLS check.
	tenantA := fmt.Sprintf("cand-it-a-%s", uuid.New().String())
	tenantB := fmt.Sprintf("cand-it-b-%s", uuid.New().String())
	if err := db.EnsureTenant(ctx, suPool, tenantA, "cand-it-a"); err != nil {
		t.Fatalf("failed to ensure tenant A: %v", err)
	}
	if err := db.EnsureTenant(ctx, suPool, tenantB, "cand-it-b"); err != nil {
		t.Fatalf("failed to ensure tenant B: %v", err)
	}

	catalogID := "cat-" + uuid.New().String()
	jobID := uuid.New().String()
	const model = "test-embed-model"

	// Seed catalog + job (superuser bypasses RLS but tenant_id is explicit).
	if err := suPool.Exec(ctx, `
		INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path)
		VALUES ($1, $2, 'IT Catalog', 'v1', 'test', 'unused')
	`, catalogID, tenantA); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	if err := suPool.Exec(ctx, `
		INSERT INTO jobs (job_id, tenant_id, status, config, created_by)
		VALUES ($1, $2, 'running', $3, 'integration-test')
	`, jobID, tenantA, []byte(`{}`)); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	// Two non-section controls (embedded) plus one section control (excluded,
	// unembedded). If the section control were counted, coverage would drop to
	// 2/3 = 0.67 < 0.8 and Generate would fail — so a passing run proves the
	// section exclusion works.
	controls := []struct {
		id, class string
	}{
		{"c1", "SC"},
		{"c2", "SC"},
		{"section-1", classifySectionClass},
	}
	for _, c := range controls {
		if err := suPool.Exec(ctx, `
			INSERT INTO controls (tenant_id, control_id, catalog_id, identifier, title, statement, class)
			VALUES ($1, $2, $3, $2, $4, $5, $6)
		`, tenantA, c.id, catalogID, "Title "+c.id, "Statement for "+c.id, c.class); err != nil {
			t.Fatalf("seed control %s: %v", c.id, err)
		}
	}

	// Identical embeddings for c1 and c2 -> cosine similarity 1.0 (scaled 100).
	vec := vectorLiteral(embeddingDim, 0.1)
	for _, id := range []string{"c1", "c2"} {
		if err := suPool.Exec(ctx, `
			INSERT INTO embeddings (catalog_id, control_id, model, vector, tenant_id)
			VALUES ($1, $2, $3, $4::vector, $5)
		`, catalogID, id, model, vec, tenantA); err != nil {
			t.Fatalf("seed embedding %s: %v", id, err)
		}
	}

	// Classify analysis_results blob (same shape classify.Aggregate persists).
	classifyBlob, err := json.Marshal([]results.ClassifyResult{
		{ControlID: "c1", Type: "Technical", Level: "Operational"},
		{ControlID: "c2", Type: "Procedural", Level: "Tactical"},
	})
	if err != nil {
		t.Fatalf("marshal classify blob: %v", err)
	}
	if err := suPool.Exec(ctx, `
		INSERT INTO analysis_results (tenant_id, job_id, analyzer_name, result_data)
		VALUES ($1, $2, 'classify', $3)
	`, tenantA, jobID, classifyBlob); err != nil {
		t.Fatalf("seed analysis_results: %v", err)
	}

	// Build the generator with real Postgres adapters.
	cfg := config.CandidateConfig{
		Generators:           []config.CandidateGeneratorEntry{{Name: "semantic", Enabled: true, Weight: 1.0}},
		MinEmbeddingCoverage: 0.8,
	}
	registry, err := BuildCandidateRegistry(cfg)
	if err != nil {
		t.Fatalf("BuildCandidateRegistry: %v", err)
	}
	gen := NewCandidateGenerator(
		&pgControlsReader{db: tenantConn},
		&pgEmbeddingsReader{db: tenantConn},
		registry,
		&pgCandidateWriter{db: tenantConn},
		cfg, model,
	)

	// Run Generate under tenant A's context.
	ctxA, err := tenant.WithTenant(ctx, tenantA)
	if err != nil {
		t.Fatalf("WithTenant A: %v", err)
	}
	jobConfig := []byte(fmt.Sprintf(`{"Source":{"CatalogId":%q}}`, catalogID))
	if err := gen.Generate(ctxA, tenantA, jobID, jobConfig); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Owner sees candidate rows in both tables.
	if n := countCandidates(t, tenantConn, ctxA, "requires_candidates", jobID); n == 0 {
		t.Fatalf("expected requires_candidates rows for owner tenant, got 0")
	}
	if n := countCandidates(t, tenantConn, ctxA, "relationship_candidates", jobID); n == 0 {
		t.Fatalf("expected relationship_candidates rows for owner tenant, got 0")
	}

	// A second tenant sees none (RLS).
	ctxB, err := tenant.WithTenant(ctx, tenantB)
	if err != nil {
		t.Fatalf("WithTenant B: %v", err)
	}
	if n := countCandidates(t, tenantConn, ctxB, "requires_candidates", jobID); n != 0 {
		t.Fatalf("RLS breach: tenant B saw %d requires_candidates rows, want 0", n)
	}
	if n := countCandidates(t, tenantConn, ctxB, "relationship_candidates", jobID); n != 0 {
		t.Fatalf("RLS breach: tenant B saw %d relationship_candidates rows, want 0", n)
	}
}

// countCandidates counts rows for a job in the given candidate table through
// the tenant-scoped connection, so RLS applies to the read.
func countCandidates(t *testing.T, conn db.TenantConnection, ctx context.Context, table, jobID string) int {
	t.Helper()
	var query string
	switch table {
	case "requires_candidates":
		query = "SELECT count(*) FROM requires_candidates WHERE job_id = $1"
	case "relationship_candidates":
		query = "SELECT count(*) FROM relationship_candidates WHERE job_id = $1"
	default:
		t.Fatalf("unsupported table %q", table)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("count %s: begin: %v", table, err)
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRow(ctx, query, jobID).Scan(&n); err != nil {
		t.Fatalf("count %s: scan: %v", table, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("count %s: commit: %v", table, err)
	}
	return n
}
