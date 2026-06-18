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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
)

func TestVectorDBIntegrationBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "VectorDB Integration Suite")
}

var testDSN string

const testVectorDim = 2000

// testVector returns a 2000-dimensional vector with the given seed values
// in the leading positions and zeros elsewhere.
func testVector(seed ...float32) []float32 {
	v := make([]float32, testVectorDim)
	copy(v, seed)
	return v
}

// Redirect slog output to GinkgoWriter so log noise only appears on failure.
var _ = BeforeEach(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

var _ = SynchronizedBeforeSuite(func() []byte {
	testDSN = os.Getenv("TEST_DATABASE_DSN")
	if testDSN == "" {
		Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
	}

	// Run migrations so the embeddings table and pgvector extension exist.
	ctx := context.Background()
	migrator, err := db.NewMigrator(testDSN)
	Expect(err).NotTo(HaveOccurred(), "failed to create migrator")
	Expect(migrator.Up(ctx)).To(Succeed(), "failed to run migrations")
	Expect(migrator.Close()).To(Succeed(), "failed to close migrator")

	// Insert FK parent rows required by the embeddings table.
	adminDB, err := sql.Open("pgx", testDSN)
	Expect(err).NotTo(HaveOccurred(), "failed to open admin connection")
	defer adminDB.Close()

	for _, t := range []struct{ id, name string }{
		{"test-tenant", "Test Tenant"},
		{"tenant-1", "Tenant One"},
		{"tenant-2", "Tenant Two"},
	} {
		_, err := adminDB.ExecContext(ctx,
			`INSERT INTO tenants (tenant_id, display_name) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			t.id, t.name)
		Expect(err).NotTo(HaveOccurred(), "failed to insert tenant %s", t.id)
	}

	for _, cid := range []string{"nist-800-53", "iso-27001"} {
		_, err := adminDB.ExecContext(ctx,
			`INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path)
			 VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`,
			cid, "test-tenant", cid, "1.0", "oscal", "test-fixture")
		Expect(err).NotTo(HaveOccurred(), "failed to insert catalog %s", cid)
	}

	return []byte(testDSN)
}, func(data []byte) {
	testDSN = string(data)
})

// cleanupTestData removes all embeddings inserted during the test run.
func cleanupTestData(sqlDB *sql.DB) {
	tenants := []string{"test-tenant", "tenant-1", "tenant-2"}
	for _, tid := range tenants {
		_, err := sqlDB.Exec(`DELETE FROM embeddings WHERE tenant_id = $1`, tid)
		if err != nil {
			fmt.Fprintf(GinkgoWriter, "cleanup warning: failed to delete embeddings for %s: %v\n", tid, err)
		}
	}
}

var _ = Describe("VectorDB Integration", func() {
	var (
		sqlDB *sql.DB
		store *vectordb.PgVectorStore
		ctx   context.Context
	)

	BeforeEach(func() {
		if testDSN == "" {
			Skip("TEST_DATABASE_DSN not set")
		}

		var err error
		sqlDB, err = sql.Open("pgx", testDSN)
		Expect(err).NotTo(HaveOccurred(), "failed to connect to database")

		store, err = vectordb.NewPgVectorStore(sqlDB)
		Expect(err).NotTo(HaveOccurred(), "failed to create vector store")

		ctx, err = tenant.WithTenant(context.Background(), "test-tenant")
		Expect(err).NotTo(HaveOccurred())

		DeferCleanup(func() {
			cleanupTestData(sqlDB)
			sqlDB.Close()
		})
	})

	Context("StoreAndRetrieve", func() {
		It("should store a single embedding", func() {
			err := store.StoreEmbedding(ctx, "test-tenant", vectordb.Embedding{
				CatalogID: "nist-800-53",
				ControlID: "AC-1",
				Model:     "text-embedding-ada-002",
				Vector:    testVector(0.1, 0.2, 0.3, 0.4, 0.5),
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should store a batch of embeddings", func() {
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
			Expect(store.StoreBatch(ctx, "test-tenant", batch)).To(Succeed())
		})
	})

	Context("SimilaritySearch", func() {
		BeforeEach(func() {
			// Ensure embeddings exist for search
			Expect(store.StoreEmbedding(ctx, "test-tenant", vectordb.Embedding{
				CatalogID: "nist-800-53",
				ControlID: "AC-1",
				Model:     "text-embedding-ada-002",
				Vector:    testVector(0.1, 0.2, 0.3, 0.4, 0.5),
			})).To(Succeed())

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
			Expect(store.StoreBatch(ctx, "test-tenant", batch)).To(Succeed())
		})

		It("should return results ordered by similarity in valid range", func() {
			results, err := store.FindSimilar(ctx, "test-tenant", vectordb.FindSimilarQuery{
				CatalogID: "nist-800-53",
				Model:     "text-embedding-ada-002",
				Vector:    testVector(0.15, 0.25, 0.35, 0.45, 0.55),
				Limit:     10,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(results).NotTo(BeEmpty())

			// Verify results are ordered by similarity (descending)
			for i := 1; i < len(results); i++ {
				Expect(results[i-1].Similarity).To(BeNumerically(">=", results[i].Similarity),
					"results not ordered by similarity: results[%d].Similarity (%f) < results[%d].Similarity (%f)",
					i-1, results[i-1].Similarity, i, results[i].Similarity)
			}

			// Verify all similarity scores are in valid range [0, 1]
			for i, r := range results {
				Expect(r.Similarity).To(BeNumerically(">=", 0),
					"results[%d].Similarity = %f, want >= 0", i, r.Similarity)
				Expect(r.Similarity).To(BeNumerically("<=", 1),
					"results[%d].Similarity = %f, want <= 1", i, r.Similarity)
			}
		})
	})

	Context("ModelIsolation", func() {
		It("should return ErrModelNotFound for a nonexistent model", func() {
			// Store an embedding with a different model
			Expect(store.StoreEmbedding(ctx, "test-tenant", vectordb.Embedding{
				CatalogID: "nist-800-53",
				ControlID: "AC-1-st",
				Model:     "sentence-transformers",
				Vector:    testVector(0.5, 0.4, 0.3, 0.2, 0.1),
			})).To(Succeed())

			// Query with a nonexistent model — must return ErrModelNotFound
			_, err := store.FindSimilar(ctx, "test-tenant", vectordb.FindSimilarQuery{
				CatalogID: "nist-800-53",
				Model:     "nonexistent-model",
				Vector:    testVector(0.1, 0.2, 0.3, 0.4, 0.5),
				Limit:     10,
			})
			Expect(errors.Is(err, vectordb.ErrModelNotFound)).To(BeTrue(),
				"expected ErrModelNotFound, got: %v", err)

			// Clean up
			Expect(store.DeleteByModel(ctx, "test-tenant", "nist-800-53", "sentence-transformers")).To(Succeed())
		})
	})

	Context("TenantIsolation", func() {
		It("should prevent tenants from seeing each other's embeddings", func() {
			ctx1, err := tenant.WithTenant(context.Background(), "tenant-1")
			Expect(err).NotTo(HaveOccurred())
			ctx2, err := tenant.WithTenant(context.Background(), "tenant-2")
			Expect(err).NotTo(HaveOccurred())

			// Store embeddings for tenant-1
			Expect(store.StoreEmbedding(ctx1, "tenant-1", vectordb.Embedding{
				CatalogID: "iso-27001",
				ControlID: "A.5.1",
				Model:     "text-embedding-ada-002",
				Vector:    testVector(0.9, 0.8, 0.7, 0.6, 0.5),
			})).To(Succeed())

			// Store embeddings for tenant-2
			Expect(store.StoreEmbedding(ctx2, "tenant-2", vectordb.Embedding{
				CatalogID: "iso-27001",
				ControlID: "A.6.1",
				Model:     "text-embedding-ada-002",
				Vector:    testVector(0.1, 0.2, 0.3, 0.4, 0.5),
			})).To(Succeed())

			// Query as tenant-1: must NOT see tenant-2's control A.6.1
			results1, err := store.FindSimilar(ctx1, "tenant-1", vectordb.FindSimilarQuery{
				CatalogID: "iso-27001",
				Model:     "text-embedding-ada-002",
				Vector:    testVector(0.1, 0.2, 0.3, 0.4, 0.5),
				Limit:     10,
			})
			Expect(err).NotTo(HaveOccurred())
			for _, r := range results1 {
				Expect(r.ControlID).NotTo(Equal("A.6.1"),
					"tenant-1 can see tenant-2's control A.6.1 — tenant isolation violated")
			}
			// Verify tenant-1 can still see their own data
			found := false
			for _, r := range results1 {
				if r.ControlID == "A.5.1" {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue(), "tenant-1 cannot see their own control A.5.1")

			// Query as tenant-2: must NOT see tenant-1's control A.5.1
			results2, err := store.FindSimilar(ctx2, "tenant-2", vectordb.FindSimilarQuery{
				CatalogID: "iso-27001",
				Model:     "text-embedding-ada-002",
				Vector:    testVector(0.9, 0.8, 0.7, 0.6, 0.5),
				Limit:     10,
			})
			Expect(err).NotTo(HaveOccurred())
			for _, r := range results2 {
				Expect(r.ControlID).NotTo(Equal("A.5.1"),
					"tenant-2 can see tenant-1's control A.5.1 — tenant isolation violated")
			}
			// Verify tenant-2 can still see their own data
			found = false
			for _, r := range results2 {
				if r.ControlID == "A.6.1" {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue(), "tenant-2 cannot see their own control A.6.1")
		})
	})
})

var _ = Describe("VectorDB Telemetry", func() {
	It("should emit spans and metrics for StoreEmbedding and FindSimilar", func() {
		if testDSN == "" {
			Skip("TEST_DATABASE_DSN not set")
		}

		sqlDB, err := sql.Open("pgx", testDSN)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cleanupTestData(sqlDB)
			sqlDB.Close()
		})

		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { tp.Shutdown(context.Background()) })

		tracer := tp.TracerProvider().Tracer("test")
		meter := tp.MeterProvider().Meter("test")

		store, err := vectordb.NewPgVectorStore(sqlDB, vectordb.WithTelemetry(tracer, meter))
		Expect(err).NotTo(HaveOccurred())

		ctx, err := tenant.WithTenant(context.Background(), "test-tenant")
		Expect(err).NotTo(HaveOccurred())

		// Exercise StoreEmbedding
		Expect(store.StoreEmbedding(ctx, "test-tenant", vectordb.Embedding{
			CatalogID: "nist-800-53",
			ControlID: "TEL-1",
			Model:     "text-embedding-ada-002",
			Vector:    testVector(0.7, 0.8, 0.9),
		})).To(Succeed())

		// Exercise FindSimilar
		_, err = store.FindSimilar(ctx, "test-tenant", vectordb.FindSimilarQuery{
			CatalogID: "nist-800-53",
			Model:     "text-embedding-ada-002",
			Vector:    testVector(0.7, 0.8, 0.9),
			Limit:     5,
		})
		Expect(err).NotTo(HaveOccurred())

		// Assert spans
		spans := tp.GetSpans()

		storeSpan := telemetrytest.FindSpan(spans, "vectordb.store_embedding")
		Expect(storeSpan).NotTo(BeNil(), "expected span vectordb.store_embedding")
		val, ok := telemetrytest.SpanAttribute(storeSpan, "tenant.id")
		Expect(ok).To(BeTrue())
		Expect(val.AsString()).To(Equal("test-tenant"))

		searchSpan := telemetrytest.FindSpan(spans, "vectordb.find_similar")
		Expect(searchSpan).NotTo(BeNil(), "expected span vectordb.find_similar")
		val, ok = telemetrytest.SpanAttribute(searchSpan, "tenant.id")
		Expect(ok).To(BeTrue())
		Expect(val.AsString()).To(Equal("test-tenant"))

		// Assert metrics
		rm := tp.GetMetrics()

		storedMetric := telemetrytest.FindMetric(rm, "vectordb.embeddings.stored.total")
		Expect(storedMetric).NotTo(BeNil(), "expected metric vectordb.embeddings.stored.total")
		storedCount, err := telemetrytest.CounterValue(storedMetric)
		Expect(err).NotTo(HaveOccurred())
		Expect(storedCount).To(BeNumerically(">=", 1))

		searchMetric := telemetrytest.FindMetric(rm, "vectordb.searches.total")
		Expect(searchMetric).NotTo(BeNil(), "expected metric vectordb.searches.total")
		searchCount, err := telemetrytest.CounterValue(searchMetric)
		Expect(err).NotTo(HaveOccurred())
		Expect(searchCount).To(BeNumerically(">=", 1))
	})
})
