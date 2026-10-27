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
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
)

func TestVectorDBIntegrationBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "VectorDB Integration Suite")
}

// testDSN is the superuser DSN, used only to migrate, seed and clean up.
// appDSN connects as the restricted app_user that crosscodexd runs as, so
// every PgVectorStore spec runs under the embeddings RLS policy.
var testDSN, appDSN string

// deletedTenant is soft-deleted and restored by the deleted-tenant specs.
const deletedTenant = "vectordb-deleted"

// tenantCatalog is the catalog each tenant other than test-tenant embeds
// against. Catalog IDs are global and an embedding may reference only a
// catalog of its own tenant, so every tenant gets its own.
var tenantCatalog = map[string]string{
	"tenant-1":    "tenant-1-catalog",
	"tenant-2":    "tenant-2-catalog",
	deletedTenant: deletedTenant + "-catalog",
}

// openAppUser opens an app_user connection closed at the end of the spec.
func openAppUser() *sql.DB {
	conn, err := sql.Open("pgx", appDSN)
	Expect(err).NotTo(HaveOccurred(), "failed to open app_user connection")
	DeferCleanup(conn.Close)
	return conn
}

// openSuperuser opens a superuser connection for seeding and verification,
// closed at the end of the spec.
func openSuperuser() *sql.DB {
	conn, err := sql.Open("pgx", testDSN)
	Expect(err).NotTo(HaveOccurred(), "failed to open superuser connection")
	DeferCleanup(conn.Close)
	return conn
}

const testVectorDim = 2000

// testVector returns a 2000-dimensional vector with the given seed values
// in the leading positions and zeros elsewhere.
func testVector(seed ...float32) []float32 {
	v := make([]float32, testVectorDim)
	copy(v, seed)
	return v
}

// dimVector returns a dim-dimensional vector with the given seed values in the
// leading positions and zeros elsewhere. Exercises embeddings of widths other
// than testVectorDim now that the embeddings column is model-agnostic.
func dimVector(dim int, seed ...float32) []float32 {
	v := make([]float32, dim)
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
	adminPool, err := db.NewPool(db.PoolConfig{DSN: testDSN})
	Expect(err).NotTo(HaveOccurred(), "failed to open admin pool")
	defer adminPool.Close()

	for _, t := range []struct{ id, name string }{
		{"test-tenant", "Test Tenant"},
		{"tenant-1", "Tenant One"},
		{"tenant-2", "Tenant Two"},
		{deletedTenant, "Deleted Tenant"},
	} {
		Expect(db.EnsureTenant(ctx, adminPool, t.id, t.name)).
			To(Succeed(), "failed to insert tenant %s", t.id)
	}

	catalogs := map[string]string{"nist-800-53": "test-tenant", "iso-27001": "test-tenant"}
	for tid, cid := range tenantCatalog {
		catalogs[cid] = tid
	}
	for cid, tid := range catalogs {
		err := adminPool.Exec(ctx,
			`INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path)
			 VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`,
			cid, tid, cid, "1.0", "oscal", "test-fixture")
		Expect(err).NotTo(HaveOccurred(), "failed to insert catalog %s", cid)
	}

	appPassword, err := dbtest.RolePassword(dbtest.AppUserPasswordEnv)
	Expect(err).NotTo(HaveOccurred(), "app_user password")
	Expect(dbtest.SetRolePassword(ctx, testDSN, "app_user", appPassword)).To(Succeed(), "failed to set app_user password")

	return []byte(testDSN)
}, func(data []byte) {
	testDSN = string(data)
	if testDSN != "" {
		appPassword, err := dbtest.RolePassword(dbtest.AppUserPasswordEnv)
		Expect(err).NotTo(HaveOccurred(), "app_user password")
		appDSN, err = dbtest.RoleDSN(testDSN, "app_user", appPassword)
		Expect(err).NotTo(HaveOccurred(), "app_user DSN")
	}
})

// cleanupTestData removes all embeddings inserted during the test run. It
// needs a superuser connection: RLS would scope app_user to one tenant.
func cleanupTestData(sqlDB *sql.DB) {
	tenants := []string{"test-tenant", "tenant-1", "tenant-2", deletedTenant}
	for _, tid := range tenants {
		_, err := sqlDB.Exec(`DELETE FROM embeddings WHERE tenant_id = $1`, tid)
		if err != nil {
			fmt.Fprintf(GinkgoWriter, "cleanup warning: failed to delete embeddings for %s: %v\n", tid, err)
		}
	}
}

var _ = Describe("VectorDB Integration", func() {
	var (
		suDB  *sql.DB
		store *vectordb.PgVectorStore
		ctx   context.Context
	)

	BeforeEach(func() {
		if testDSN == "" {
			Skip("TEST_DATABASE_DSN not set")
		}

		suDB = openSuperuser()
		DeferCleanup(func() { cleanupTestData(suDB) })

		var err error
		store, err = vectordb.NewPgVectorStore(openAppUser())
		Expect(err).NotTo(HaveOccurred(), "failed to create vector store")

		ctx, err = tenant.WithTenant(context.Background(), "test-tenant")
		Expect(err).NotTo(HaveOccurred())
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
		BeforeEach(func() {
			for _, e := range []struct{ tenantID, controlID string }{
				{"tenant-1", "A.5.1"},
				{"tenant-2", "A.6.1"},
			} {
				tctx, err := tenant.WithTenant(context.Background(), e.tenantID)
				Expect(err).NotTo(HaveOccurred())
				Expect(store.StoreEmbedding(tctx, e.tenantID, vectordb.Embedding{
					CatalogID: tenantCatalog[e.tenantID],
					ControlID: e.controlID,
					Model:     "text-embedding-ada-002",
					Vector:    testVector(0.5, 0.5, 0.5),
				})).To(Succeed())
			}
		})

		DescribeTable("should prevent tenants from seeing each other's embeddings",
			func(own, other, ownControl string) {
				tctx, err := tenant.WithTenant(context.Background(), own)
				Expect(err).NotTo(HaveOccurred())
				query := func(catalogID string) ([]vectordb.SimilarityResult, error) {
					return store.FindSimilar(tctx, own, vectordb.FindSimilarQuery{
						CatalogID: catalogID,
						Model:     "text-embedding-ada-002",
						Vector:    testVector(0.5, 0.5, 0.5),
						Limit:     10,
					})
				}

				results, err := query(tenantCatalog[own])
				Expect(err).NotTo(HaveOccurred())
				Expect(results).To(HaveLen(1), "%s must see exactly its own embedding", own)
				Expect(results[0].ControlID).To(Equal(ownControl))

				_, err = query(tenantCatalog[other])
				Expect(err).To(MatchError(vectordb.ErrModelNotFound),
					"%s can see %s's catalog — tenant isolation violated", own, other)
			},
			Entry("tenant-1", "tenant-1", "tenant-2", "A.5.1"),
			Entry("tenant-2", "tenant-2", "tenant-1", "A.6.1"),
		)
	})

	Context("ModelAgnosticDimensions", func() {
		It("persists an embedding whose width is not testVectorDim (384)", func() {
			err := store.StoreEmbedding(ctx, "test-tenant", vectordb.Embedding{
				CatalogID: "nist-800-53",
				ControlID: "AC-1",
				Model:     "granite-embedding:30m",
				Vector:    dimVector(384, 0.1, 0.2, 0.3),
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("MultiModelCoexistence", func() {
		const (
			modelA = "granite-embedding:30m" // 384-dim
			modelB = "nomic-embed-text"      // 768-dim
		)

		BeforeEach(func() {
			Expect(store.StoreEmbedding(ctx, "test-tenant", vectordb.Embedding{
				CatalogID: "nist-800-53", ControlID: "AC-1", Model: modelA,
				Vector: dimVector(384, 0.1, 0.2, 0.3),
			})).To(Succeed())
			Expect(store.StoreEmbedding(ctx, "test-tenant", vectordb.Embedding{
				CatalogID: "nist-800-53", ControlID: "AC-1", Model: modelB,
				Vector: dimVector(768, 0.4, 0.5, 0.6),
			})).To(Succeed())
		})

		It("stores different-dimension models under the same catalog+control", func() {
			results, err := store.FindSimilar(ctx, "test-tenant", vectordb.FindSimilarQuery{
				CatalogID: "nist-800-53", Model: modelA,
				Vector: dimVector(384, 0.1, 0.2, 0.3), Limit: 10,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].ControlID).To(Equal("AC-1"))
		})

		It("FindSimilar for model B returns only model B rows at its own width", func() {
			results, err := store.FindSimilar(ctx, "test-tenant", vectordb.FindSimilarQuery{
				CatalogID: "nist-800-53", Model: modelB,
				Vector: dimVector(768, 0.4, 0.5, 0.6), Limit: 10,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].ControlID).To(Equal("AC-1"))
		})

		It("rejects a query whose width does not match the target model", func() {
			_, err := store.FindSimilar(ctx, "test-tenant", vectordb.FindSimilarQuery{
				CatalogID: "nist-800-53", Model: modelA,
				Vector: dimVector(768, 0.4), Limit: 10,
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("dimension"))
		})
	})
})

// countEmbeddings counts tenantID's embeddings through a superuser
// connection, which RLS does not filter, so a negative spec can prove the
// data is unchanged regardless of what app_user can see.
func countEmbeddings(suDB *sql.DB, tenantID string) int {
	var n int
	Expect(suDB.QueryRow(`SELECT COUNT(*) FROM embeddings WHERE tenant_id = $1`, tenantID).Scan(&n)).To(Succeed())
	return n
}

var _ = Describe("VectorDB row-level security", func() {
	var (
		suDB  *sql.DB
		store *vectordb.PgVectorStore
	)

	tenantCtx := func(id string) context.Context {
		ctx, err := tenant.WithTenant(context.Background(), id)
		Expect(err).NotTo(HaveOccurred())
		return ctx
	}

	BeforeEach(func() {
		if testDSN == "" {
			Skip("TEST_DATABASE_DSN not set")
		}
		suDB = openSuperuser()
		DeferCleanup(func() { cleanupTestData(suDB) })

		var err error
		store, err = vectordb.NewPgVectorStore(openAppUser())
		Expect(err).NotTo(HaveOccurred())
	})

	It("stores, finds, gets, counts and deletes a tenant's own embeddings", func() {
		ctx := tenantCtx("tenant-1")
		Expect(store.StoreEmbedding(ctx, "tenant-1", vectordb.Embedding{
			CatalogID: tenantCatalog["tenant-1"], ControlID: "RLS-1", Model: "rls-model", Vector: testVector(0.1, 0.2),
		})).To(Succeed())
		Expect(store.StoreBatch(ctx, "tenant-1", []vectordb.Embedding{
			{CatalogID: tenantCatalog["tenant-1"], ControlID: "RLS-2", Model: "rls-model", Vector: testVector(0.2, 0.1)},
		})).To(Succeed())

		results, err := store.FindSimilar(ctx, "tenant-1", vectordb.FindSimilarQuery{
			CatalogID: tenantCatalog["tenant-1"], Model: "rls-model", Vector: testVector(0.1, 0.2), Limit: 10,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(results).To(HaveLen(2))

		got, err := store.Get(ctx, "RLS-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.ID).To(Equal("RLS-1"))
		Expect(got.Vector).To(Equal(testVector(0.1, 0.2)))

		Expect(store.Count(ctx)).To(Equal(int64(2)))

		Expect(store.Delete(ctx, "RLS-1")).To(Succeed())
		Expect(store.DeleteByModel(ctx, "tenant-1", tenantCatalog["tenant-1"], "rls-model")).To(Succeed())
		Expect(store.Count(ctx)).To(Equal(int64(0)))
		Expect(countEmbeddings(suDB, "tenant-1")).To(Equal(0))
	})

	Context("when another tenant reaches for tenant-1's embeddings", func() {
		BeforeEach(func() {
			Expect(store.StoreEmbedding(tenantCtx("tenant-1"), "tenant-1", vectordb.Embedding{
				CatalogID: tenantCatalog["tenant-1"], ControlID: "RLS-A", Model: "rls-model", Vector: testVector(0.3, 0.4),
			})).To(Succeed())
		})

		It("hides them from FindSimilar, Get and Count", func() {
			ctx := tenantCtx("tenant-2")
			_, err := store.FindSimilar(ctx, "tenant-2", vectordb.FindSimilarQuery{
				CatalogID: tenantCatalog["tenant-1"], Model: "rls-model", Vector: testVector(0.3, 0.4), Limit: 10,
			})
			Expect(err).To(MatchError(vectordb.ErrModelNotFound))

			_, err = store.Get(ctx, "RLS-A")
			Expect(err).To(MatchError(vectordb.ErrNotFound))

			Expect(store.Count(ctx)).To(Equal(int64(0)))
		})

		It("refuses Delete with ErrNotFound and leaves them unchanged", func() {
			err := store.Delete(tenantCtx("tenant-2"), "RLS-A")
			Expect(err).To(MatchError(vectordb.ErrNotFound))
			Expect(countEmbeddings(suDB, "tenant-1")).To(Equal(1))
		})

		It("deletes nothing through DeleteByModel", func() {
			Expect(store.DeleteByModel(tenantCtx("tenant-2"), "tenant-2", tenantCatalog["tenant-1"], "rls-model")).To(Succeed())
			Expect(countEmbeddings(suDB, "tenant-1")).To(Equal(1))
		})
	})

	// The embeddings key includes tenant_id, so two tenants embedding the
	// same control with the same model each own a row; an upsert never
	// takes over another tenant's row. Each tenant embeds against its own
	// catalog, the only kind it may reference, so the two
	// keys differ in catalog_id too; pkg/db's "embeddings tenant key" spec
	// proves the key itself leads with tenant_id.
	Context("when two tenants store the same control and model", func() {
		shared := func(tenantID string, vector []float32) vectordb.Embedding {
			return vectordb.Embedding{CatalogID: tenantCatalog[tenantID], ControlID: "RLS-SHARED", Model: "rls-model", Vector: vector}
		}
		vectorOf := func(id string) []float32 {
			got, err := store.Get(tenantCtx(id), "RLS-SHARED")
			Expect(err).NotTo(HaveOccurred(), "Get for %s", id)
			return got.Vector
		}
		storeOne := func(ctx context.Context, tenantID string, e vectordb.Embedding) error {
			return store.StoreEmbedding(ctx, tenantID, e)
		}
		storeAsBatch := func(ctx context.Context, tenantID string, e vectordb.Embedding) error {
			return store.StoreBatch(ctx, tenantID, []vectordb.Embedding{e})
		}

		DescribeTable("keeps a row per tenant, and a re-store updates only its own",
			func(store1, store2 func(ctx context.Context, tenantID string, e vectordb.Embedding) error) {
				Expect(store1(tenantCtx("tenant-1"), "tenant-1", shared("tenant-1", testVector(0.1)))).To(Succeed())
				Expect(store2(tenantCtx("tenant-2"), "tenant-2", shared("tenant-2", testVector(0.9)))).To(Succeed())
				Expect(countEmbeddings(suDB, "tenant-1")).To(Equal(1))
				Expect(countEmbeddings(suDB, "tenant-2")).To(Equal(1))
				Expect(vectorOf("tenant-1")).To(Equal(testVector(0.1)))
				Expect(vectorOf("tenant-2")).To(Equal(testVector(0.9)))

				Expect(store1(tenantCtx("tenant-1"), "tenant-1", shared("tenant-1", testVector(0.5)))).To(Succeed())
				Expect(countEmbeddings(suDB, "tenant-1")).To(Equal(1))
				Expect(countEmbeddings(suDB, "tenant-2")).To(Equal(1))
				Expect(vectorOf("tenant-1")).To(Equal(testVector(0.5)))
				Expect(vectorOf("tenant-2")).To(Equal(testVector(0.9)))
			},
			Entry("StoreEmbedding", storeOne, storeOne),
			Entry("StoreBatch", storeAsBatch, storeAsBatch),
			Entry("StoreEmbedding then StoreBatch", storeOne, storeAsBatch),
		)
	})

	// Foreign keys ignore RLS, so only the composite catalog foreign key
	// (embeddings_catalog_tenant_fkey) stops a tenant embedding against another tenant's
	// catalog. A batch holding one such embedding is refused whole.
	DescribeTable("refuses an embedding against another tenant's catalog and writes nothing",
		func(op func(ctx context.Context) error) {
			err := op(tenantCtx("tenant-1"))
			Expect(err).To(MatchError(ContainSubstring(fmt.Sprintf(
				"catalog %q is not a catalog of tenant %q; store embeddings only for the tenant's own catalogs",
				tenantCatalog["tenant-2"], "tenant-1"))))
			Expect(db.IsPgErrorCode(err, "23503")).To(BeTrue(), "want the foreign_key_violation wrapped, got %v", err)
			Expect(countEmbeddings(suDB, "tenant-1")).To(BeZero())
			Expect(countEmbeddings(suDB, "tenant-2")).To(BeZero())
		},
		Entry("StoreEmbedding", func(ctx context.Context) error {
			return store.StoreEmbedding(ctx, "tenant-1", vectordb.Embedding{
				CatalogID: tenantCatalog["tenant-2"], ControlID: "RLS-X", Model: "rls-model", Vector: testVector(0.7),
			})
		}),
		Entry("StoreBatch", func(ctx context.Context) error {
			return store.StoreBatch(ctx, "tenant-1", []vectordb.Embedding{
				{CatalogID: tenantCatalog["tenant-1"], ControlID: "RLS-OWN", Model: "rls-model", Vector: testVector(0.7)},
				{CatalogID: tenantCatalog["tenant-2"], ControlID: "RLS-X", Model: "rls-model", Vector: testVector(0.7)},
			})
		}),
		Entry("Insert", func(ctx context.Context) error {
			return store.Insert(ctx, "RLS-X", testVector(0.7), map[string]any{
				"catalog_id": tenantCatalog["tenant-2"], "model": "rls-model",
			})
		}),
	)

	Context("when the tenant is deleted", func() {
		// The tenant is soft-deleted with a superuser UPDATE: delete_tenant
		// needs a signed attestation, which this suite has no key for, and
		// the row state is all enter_tenant reads.
		setDeleted := func(deleted bool) {
			stmt := "UPDATE tenants SET status = 'active', deleted_at = NULL, deleted_by = NULL WHERE tenant_id = $1"
			if deleted {
				stmt = "UPDATE tenants SET status = 'deleted', deleted_at = now(), deleted_by = 'test' WHERE tenant_id = $1"
			}
			_, err := suDB.Exec(stmt, deletedTenant)
			Expect(err).NotTo(HaveOccurred())
		}

		BeforeEach(func() {
			Expect(store.StoreEmbedding(tenantCtx(deletedTenant), deletedTenant, vectordb.Embedding{
				CatalogID: tenantCatalog[deletedTenant], ControlID: "RLS-KEPT", Model: "rls-model", Vector: testVector(0.5),
			})).To(Succeed())
			setDeleted(true)
			DeferCleanup(func() { setDeleted(false) })
		})

		// expectRefused asserts the enter_tenant refusal: SQLSTATE 42501
		// with the restore command an operator needs.
		expectRefused := func(err error) {
			Expect(err).To(HaveOccurred())
			Expect(db.IsPgErrorCode(err, "42501")).To(BeTrue(), "want insufficient_privilege, got %v", err)
			Expect(err.Error()).To(ContainSubstring("admin tenant restore"))
		}

		DescribeTable("refuses every operation and leaves the data unchanged",
			func(op func(ctx context.Context) error) {
				expectRefused(op(tenantCtx(deletedTenant)))
				Expect(countEmbeddings(suDB, deletedTenant)).To(Equal(1))
			},
			Entry("StoreEmbedding", func(ctx context.Context) error {
				return store.StoreEmbedding(ctx, deletedTenant, vectordb.Embedding{
					CatalogID: tenantCatalog[deletedTenant], ControlID: "RLS-NEW", Model: "rls-model", Vector: testVector(0.6),
				})
			}),
			Entry("StoreBatch", func(ctx context.Context) error {
				return store.StoreBatch(ctx, deletedTenant, []vectordb.Embedding{
					{CatalogID: tenantCatalog[deletedTenant], ControlID: "RLS-NEW", Model: "rls-model", Vector: testVector(0.6)},
				})
			}),
			Entry("FindSimilar", func(ctx context.Context) error {
				_, err := store.FindSimilar(ctx, deletedTenant, vectordb.FindSimilarQuery{
					CatalogID: tenantCatalog[deletedTenant], Model: "rls-model", Vector: testVector(0.5), Limit: 10,
				})
				return err
			}),
			Entry("Get", func(ctx context.Context) error {
				_, err := store.Get(ctx, "RLS-KEPT")
				return err
			}),
			Entry("Count", func(ctx context.Context) error {
				_, err := store.Count(ctx)
				return err
			}),
			Entry("Delete", func(ctx context.Context) error {
				return store.Delete(ctx, "RLS-KEPT")
			}),
			Entry("DeleteByModel", func(ctx context.Context) error {
				return store.DeleteByModel(ctx, deletedTenant, tenantCatalog[deletedTenant], "rls-model")
			}),
		)
	})
})

var _ = Describe("VectorDB Telemetry", func() {
	It("should emit spans and metrics for StoreEmbedding and FindSimilar", func() {
		if testDSN == "" {
			Skip("TEST_DATABASE_DSN not set")
		}

		suDB := openSuperuser()
		DeferCleanup(func() { cleanupTestData(suDB) })

		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(tp.Shutdown)

		tracer := tp.TracerProvider().Tracer("test")
		meter := tp.MeterProvider().Meter("test")

		store, err := vectordb.NewPgVectorStore(openAppUser(), vectordb.WithTelemetry(tracer, meter))
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
