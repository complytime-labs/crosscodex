//go:build integration

package vectordb_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
)

var testDSN string

const testVectorDim = 2000

// testVector returns a 2000-dimensional vector with the given seed values
// in the leading positions and zeros elsewhere. The embeddings column is
// vector(2000), so all test vectors must match that dimension.
func testVector(seed ...float32) []float32 {
	v := make([]float32, testVectorDim)
	copy(v, seed)
	return v
}

func TestMain(m *testing.M) {
	testDSN = os.Getenv("TEST_DATABASE_DSN")
	if testDSN == "" {
		fmt.Fprintln(os.Stderr, "TEST_DATABASE_DSN not set — run: task dev:test-integration")
		os.Exit(1)
	}

	// Run migrations so the embeddings table and pgvector extension exist.
	ctx := context.Background()
	migrator, err := db.NewMigrator(testDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create migrator: %v\n", err)
		os.Exit(1)
	}
	if err := migrator.Up(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "failed to run migrations: %v\n", err)
		os.Exit(1)
	}
	if err := migrator.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to close migrator: %v\n", err)
		os.Exit(1)
	}

	// Insert FK parent rows required by the embeddings table.
	// The embeddings table references tenants(tenant_id) and catalogs(catalog_id).
	adminDB, err := sql.Open("pgx", testDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open admin connection: %v\n", err)
		os.Exit(1)
	}
	defer adminDB.Close()

	for _, t := range []struct{ id, name string }{
		{"test-tenant", "Test Tenant"},
		{"tenant-1", "Tenant One"},
		{"tenant-2", "Tenant Two"},
	} {
		if _, err := adminDB.ExecContext(ctx,
			`INSERT INTO tenants (tenant_id, display_name) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			t.id, t.name); err != nil {
			fmt.Fprintf(os.Stderr, "failed to insert tenant %s: %v\n", t.id, err)
			os.Exit(1)
		}
	}

	for _, cid := range []string{"nist-800-53", "iso-27001"} {
		if _, err := adminDB.ExecContext(ctx,
			`INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path)
			 VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`,
			cid, "test-tenant", cid, "1.0", "oscal", "test-fixture"); err != nil {
			fmt.Fprintf(os.Stderr, "failed to insert catalog %s: %v\n", cid, err)
			os.Exit(1)
		}
	}

	os.Exit(m.Run())
}

func TestVectorDBIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	sqlDB, err := sql.Open("pgx", testDSN)
	if err != nil {
		t.Fatalf("failed to connect to database: %v", err)
	}

	store, err := vectordb.NewPgVectorStore(sqlDB)
	if err != nil {
		t.Fatalf("failed to create vector store: %v", err)
	}

	ctx := tenant.WithTenant(context.Background(), "test-tenant")

	// t.Cleanup runs in LIFO order. Register Close first so it runs last,
	// after the data cleanup has finished using the connection.
	t.Cleanup(func() { sqlDB.Close() })
	t.Cleanup(func() { cleanupTestData(t, sqlDB) })

	t.Run("StoreAndRetrieve", func(t *testing.T) {
		testStoreAndRetrieve(t, store, ctx)
	})
	t.Run("SimilaritySearch", func(t *testing.T) {
		testSimilaritySearch(t, store, ctx)
	})
	t.Run("ModelIsolation", func(t *testing.T) {
		testModelIsolation(t, store, ctx)
	})
	t.Run("TenantIsolation", func(t *testing.T) {
		testTenantIsolation(t, store, sqlDB)
	})
}

// cleanupTestData removes all embeddings inserted during the test run.
func cleanupTestData(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	tenants := []string{"test-tenant", "tenant-1", "tenant-2"}
	for _, tid := range tenants {
		_, err := sqlDB.Exec(`DELETE FROM embeddings WHERE tenant_id = $1`, tid)
		if err != nil {
			t.Logf("cleanup warning: failed to delete embeddings for %s: %v", tid, err)
		}
	}
}

// testStoreAndRetrieve verifies single and batch embedding storage.
func testStoreAndRetrieve(t *testing.T, store *vectordb.PgVectorStore, ctx context.Context) {
	t.Helper()

	// Store a single embedding for NIST 800-53 AC-1
	err := store.StoreEmbedding(ctx, "test-tenant", vectordb.Embedding{
		CatalogID: "nist-800-53",
		ControlID: "AC-1",
		Model:     "text-embedding-ada-002",
		Vector:    testVector(0.1, 0.2, 0.3, 0.4, 0.5),
	})
	if err != nil {
		t.Fatalf("StoreEmbedding failed: %v", err)
	}

	// Store a batch of 2 embeddings (AC-2 and AC-3)
	batch := []vectordb.Embedding{
		{
			CatalogID: "nist-800-53",
			ControlID: "AC-2",
			Model:     "text-embedding-ada-002",
			Vector:    testVector(0.2, 0.3, 0.4, 0.5, 0.6),
		},
		{
			CatalogID: "nist-800-53",
			ControlID: "AC-3",
			Model:     "text-embedding-ada-002",
			Vector:    testVector(0.3, 0.4, 0.5, 0.6, 0.7),
		},
	}
	err = store.StoreBatch(ctx, "test-tenant", batch)
	if err != nil {
		t.Fatalf("StoreBatch failed: %v", err)
	}
}

// testSimilaritySearch verifies search results are ordered by similarity
// and scores fall within the valid [0, 1] range.
func testSimilaritySearch(t *testing.T, store *vectordb.PgVectorStore, ctx context.Context) {
	t.Helper()

	// Query with a vector close to AC-1's embedding
	results, err := store.FindSimilar(ctx, "test-tenant", vectordb.FindSimilarQuery{
		CatalogID: "nist-800-53",
		Model:     "text-embedding-ada-002",
		Vector:    testVector(0.15, 0.25, 0.35, 0.45, 0.55),
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("FindSimilar failed: %v", err)
	}

	if len(results) < 1 {
		t.Fatal("expected at least 1 result, got 0")
	}

	// Verify results are ordered by similarity (descending)
	for i := 1; i < len(results); i++ {
		if results[i-1].Similarity < results[i].Similarity {
			t.Errorf("results not ordered by similarity: results[%d].Similarity (%f) < results[%d].Similarity (%f)",
				i-1, results[i-1].Similarity, i, results[i].Similarity)
		}
	}

	// Verify all similarity scores are in valid range [0, 1]
	for i, r := range results {
		if r.Similarity < 0 || r.Similarity > 1 {
			t.Errorf("results[%d].Similarity = %f, want value in [0, 1]", i, r.Similarity)
		}
	}
}

// testModelIsolation verifies that querying a nonexistent model returns
// ErrModelNotFound, not a silent empty result.
func testModelIsolation(t *testing.T, store *vectordb.PgVectorStore, ctx context.Context) {
	t.Helper()

	// Store an embedding with a different model
	err := store.StoreEmbedding(ctx, "test-tenant", vectordb.Embedding{
		CatalogID: "nist-800-53",
		ControlID: "AC-1-st",
		Model:     "sentence-transformers",
		Vector:    testVector(0.5, 0.4, 0.3, 0.2, 0.1),
	})
	if err != nil {
		t.Fatalf("StoreEmbedding (sentence-transformers) failed: %v", err)
	}

	// Query with a nonexistent model — must return ErrModelNotFound
	_, err = store.FindSimilar(ctx, "test-tenant", vectordb.FindSimilarQuery{
		CatalogID: "nist-800-53",
		Model:     "nonexistent-model",
		Vector:    testVector(0.1, 0.2, 0.3, 0.4, 0.5),
		Limit:     10,
	})
	if !errors.Is(err, vectordb.ErrModelNotFound) {
		t.Fatalf("expected ErrModelNotFound, got: %v", err)
	}

	// Clean up: remove sentence-transformers embeddings
	err = store.DeleteByModel(ctx, "test-tenant", "nist-800-53", "sentence-transformers")
	if err != nil {
		t.Fatalf("DeleteByModel failed: %v", err)
	}
}

// testTenantIsolation verifies that tenants cannot see each other's embeddings.
// Uses separate tenant contexts and asserts cross-tenant data is invisible.
func testTenantIsolation(t *testing.T, store *vectordb.PgVectorStore, sqlDB *sql.DB) {
	t.Helper()

	ctx1 := tenant.WithTenant(context.Background(), "tenant-1")
	ctx2 := tenant.WithTenant(context.Background(), "tenant-2")

	// Store embeddings for tenant-1
	err := store.StoreEmbedding(ctx1, "tenant-1", vectordb.Embedding{
		CatalogID: "iso-27001",
		ControlID: "A.5.1",
		Model:     "text-embedding-ada-002",
		Vector:    testVector(0.9, 0.8, 0.7, 0.6, 0.5),
	})
	if err != nil {
		t.Fatalf("StoreEmbedding (tenant-1) failed: %v", err)
	}

	// Store embeddings for tenant-2
	err = store.StoreEmbedding(ctx2, "tenant-2", vectordb.Embedding{
		CatalogID: "iso-27001",
		ControlID: "A.6.1",
		Model:     "text-embedding-ada-002",
		Vector:    testVector(0.1, 0.2, 0.3, 0.4, 0.5),
	})
	if err != nil {
		t.Fatalf("StoreEmbedding (tenant-2) failed: %v", err)
	}

	// Query as tenant-1: must NOT see tenant-2's control A.6.1
	results1, err := store.FindSimilar(ctx1, "tenant-1", vectordb.FindSimilarQuery{
		CatalogID: "iso-27001",
		Model:     "text-embedding-ada-002",
		Vector:    testVector(0.1, 0.2, 0.3, 0.4, 0.5),
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("FindSimilar (tenant-1) failed: %v", err)
	}
	for _, r := range results1 {
		if r.ControlID == "A.6.1" {
			t.Error("tenant-1 can see tenant-2's control A.6.1 — tenant isolation violated")
		}
	}
	// Verify tenant-1 can still see their own data
	found := false
	for _, r := range results1 {
		if r.ControlID == "A.5.1" {
			found = true
			break
		}
	}
	if !found {
		t.Error("tenant-1 cannot see their own control A.5.1")
	}

	// Query as tenant-2: must NOT see tenant-1's control A.5.1
	results2, err := store.FindSimilar(ctx2, "tenant-2", vectordb.FindSimilarQuery{
		CatalogID: "iso-27001",
		Model:     "text-embedding-ada-002",
		Vector:    testVector(0.9, 0.8, 0.7, 0.6, 0.5),
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("FindSimilar (tenant-2) failed: %v", err)
	}
	for _, r := range results2 {
		if r.ControlID == "A.5.1" {
			t.Error("tenant-2 can see tenant-1's control A.5.1 — tenant isolation violated")
		}
	}
	// Verify tenant-2 can still see their own data
	found = false
	for _, r := range results2 {
		if r.ControlID == "A.6.1" {
			found = true
			break
		}
	}
	if !found {
		t.Error("tenant-2 cannot see their own control A.6.1")
	}
}
