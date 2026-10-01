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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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

// countCandidates counts rows for a job in the given candidate table through
// the tenant-scoped connection, so RLS applies to the read. Offset 1 so a
// failure here is reported at the caller's line.
func countCandidates(conn db.TenantConnection, ctx context.Context, table, jobID string) int {
	var query string
	switch table {
	case "requires_candidates":
		query = "SELECT count(*) FROM requires_candidates WHERE job_id = $1"
	case "relationship_candidates":
		query = "SELECT count(*) FROM relationship_candidates WHERE job_id = $1"
	default:
		Fail(fmt.Sprintf("unsupported table %q", table), 1)
	}
	tx, err := conn.Begin(ctx)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "count %s: begin", table)
	defer func() { _ = tx.Rollback() }()
	var n int
	ExpectWithOffset(1, tx.QueryRow(ctx, query, jobID).Scan(&n)).To(Succeed(), "count %s: scan", table)
	ExpectWithOffset(1, tx.Commit()).To(Succeed(), "count %s: commit", table)
	return n
}

// TestCandidateGenerationIntegration exercises the full CandidateGenerator with
// the real Postgres-backed pgControlsReader/pgEmbeddingsReader/pgCandidateWriter
// against a live db (db profile). It seeds a catalog, controls (including one
// excluded section control), a classify analysis_results blob, and embeddings,
// runs Generate, and asserts requires_candidates/relationship_candidates rows
// exist for the owning tenant while a second tenant sees none (RLS).
var _ = Describe("CandidateGenerator integration", func() {
	It("persists requires/relationship candidates for the owning tenant and isolates a second tenant via RLS", func() {
		ctx := context.Background()

		suDSN := os.Getenv("TEST_DATABASE_DSN")
		if suDSN == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		// Migrate (idempotent).
		migrator, err := db.NewMigrator(suDSN)
		Expect(err).NotTo(HaveOccurred(), "failed to create migrator")
		Expect(migrator.Up(ctx)).To(Succeed(), "failed to run migrations")
		Expect(migrator.Close()).To(Succeed(), "failed to close migrator")

		// Ensure app_user password (idempotent).
		adminDB, err := sql.Open("pgx", suDSN)
		Expect(err).NotTo(HaveOccurred(), "failed to open admin connection")
		_, err = adminDB.ExecContext(ctx, "ALTER ROLE app_user WITH PASSWORD 'apppass'")
		Expect(err).NotTo(HaveOccurred(), "failed to set app_user password")
		Expect(adminDB.Close()).To(Succeed(), "failed to close admin connection")

		// Superuser pool for RLS-bypassing seed writes.
		suPool, err := db.NewPool(db.PoolConfig{
			DSN:          suDSN,
			MaxOpenConns: 5,
			Extensions:   []string{"age", "vector"},
		})
		Expect(err).NotTo(HaveOccurred(), "failed to create superuser pool")
		defer suPool.Close()

		// app_user pool -> tenant-scoped connection (RLS enforced).
		appDSN, err := candidateITAppUserDSN(suDSN)
		Expect(err).NotTo(HaveOccurred(), "failed to build app_user DSN")
		appPool, err := db.NewPool(db.PoolConfig{DSN: appDSN, MaxOpenConns: 2})
		Expect(err).NotTo(HaveOccurred(), "failed to create app_user pool")
		defer appPool.Close()
		tenantConn := db.NewTenantPool(appPool)

		// Two tenants: the owner and an unrelated tenant used for the RLS check.
		tenantA := fmt.Sprintf("cand-it-a-%s", uuid.New().String())
		tenantB := fmt.Sprintf("cand-it-b-%s", uuid.New().String())
		Expect(db.EnsureTenant(ctx, suPool, tenantA, "cand-it-a")).To(Succeed(), "failed to ensure tenant A")
		Expect(db.EnsureTenant(ctx, suPool, tenantB, "cand-it-b")).To(Succeed(), "failed to ensure tenant B")

		catalogID := "cat-" + uuid.New().String()
		jobID := uuid.New().String()
		const model = "test-embed-model"

		// Seed catalog + job (superuser bypasses RLS but tenant_id is explicit).
		Expect(suPool.Exec(ctx, `
			INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path)
			VALUES ($1, $2, 'IT Catalog', 'v1', 'test', 'unused')
		`, catalogID, tenantA)).To(Succeed(), "seed catalog")
		Expect(suPool.Exec(ctx, `
			INSERT INTO jobs (job_id, tenant_id, status, config, created_by)
			VALUES ($1, $2, 'running', $3, 'integration-test')
		`, jobID, tenantA, []byte(`{}`))).To(Succeed(), "seed job")

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
			Expect(suPool.Exec(ctx, `
				INSERT INTO controls (tenant_id, control_id, catalog_id, identifier, title, statement, class)
				VALUES ($1, $2, $3, $2, $4, $5, $6)
			`, tenantA, c.id, catalogID, "Title "+c.id, "Statement for "+c.id, c.class)).To(Succeed(), "seed control %s", c.id)
		}

		// Identical embeddings for c1 and c2 -> cosine similarity 1.0 (scaled 100).
		vec := vectorLiteral(embeddingDim, 0.1)
		for _, id := range []string{"c1", "c2"} {
			Expect(suPool.Exec(ctx, `
				INSERT INTO embeddings (catalog_id, control_id, model, vector, tenant_id)
				VALUES ($1, $2, $3, $4::vector, $5)
			`, catalogID, id, model, vec, tenantA)).To(Succeed(), "seed embedding %s", id)
		}

		// Classify analysis_results blob (same shape classify.Aggregate persists).
		classifyBlob, err := json.Marshal([]results.ClassifyResult{
			{ControlID: "c1", Type: "Technical", Level: "Operational"},
			{ControlID: "c2", Type: "Procedural", Level: "Tactical"},
		})
		Expect(err).NotTo(HaveOccurred(), "marshal classify blob")
		Expect(suPool.Exec(ctx, `
			INSERT INTO analysis_results (tenant_id, job_id, analyzer_name, result_data)
			VALUES ($1, $2, 'classify', $3)
		`, tenantA, jobID, classifyBlob)).To(Succeed(), "seed analysis_results")

		// Build the generator with real Postgres adapters.
		cfg := config.CandidateConfig{
			Generators:           []config.CandidateGeneratorEntry{{Name: "semantic", Enabled: true, Weight: 1.0}},
			MinEmbeddingCoverage: 0.8,
		}
		registry, err := BuildCandidateRegistry(cfg, nil, nil)
		Expect(err).NotTo(HaveOccurred(), "BuildCandidateRegistry")
		gen := NewCandidateGenerator(
			&pgControlsReader{db: tenantConn},
			&pgEmbeddingsReader{db: tenantConn},
			registry,
			&pgCandidateWriter{db: tenantConn},
			cfg, model,
		)

		// Run Generate under tenant A's context.
		ctxA, err := tenant.WithTenant(ctx, tenantA)
		Expect(err).NotTo(HaveOccurred(), "WithTenant A")
		jobConfig := []byte(fmt.Sprintf(`{"Source":{"CatalogId":%q}}`, catalogID))
		Expect(gen.Generate(ctxA, tenantA, jobID, jobConfig)).To(Succeed(), "Generate")

		// Owner sees candidate rows in both tables.
		Expect(countCandidates(tenantConn, ctxA, "requires_candidates", jobID)).NotTo(BeZero(), "expected requires_candidates rows for owner tenant")
		Expect(countCandidates(tenantConn, ctxA, "relationship_candidates", jobID)).NotTo(BeZero(), "expected relationship_candidates rows for owner tenant")

		// A second tenant sees none (RLS).
		ctxB, err := tenant.WithTenant(ctx, tenantB)
		Expect(err).NotTo(HaveOccurred(), "WithTenant B")
		Expect(countCandidates(tenantConn, ctxB, "requires_candidates", jobID)).To(BeZero(), "RLS breach: tenant B saw requires_candidates rows")
		Expect(countCandidates(tenantConn, ctxB, "relationship_candidates", jobID)).To(BeZero(), "RLS breach: tenant B saw relationship_candidates rows")
	})
})
