//go:build !integration

package vectordb_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
)

func TestVectorDBBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "VectorDB BDD Suite")
}

// Redirect slog output to GinkgoWriter so log noise only appears on failure.
var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

// newTestStore creates a PgVectorStore with a zero-value sql.DB for unit tests
// that only exercise pre-database validation paths.
func newTestStore(opts ...vectordb.Option) *vectordb.PgVectorStore {
	store, err := vectordb.NewPgVectorStore(&sql.DB{}, opts...)
	Expect(err).NotTo(HaveOccurred(), "NewPgVectorStore should not fail with a valid sql.DB")
	return store
}

// mismatchCtx returns a context with "context-tenant" set, intended for use
// with a param tenant of "param-tenant" to trigger a mismatch error.
// Panics if the tenant ID is invalid — test helpers must use valid IDs.
func mismatchCtx() context.Context {
	ctx, err := tenant.WithTenant(context.Background(), "context-tenant")
	if err != nil {
		panic(fmt.Sprintf("mismatchCtx: %v", err))
	}
	return ctx
}

// testFindSimilarQuery returns a FindSimilarQuery with standard test values.
func testFindSimilarQuery() vectordb.FindSimilarQuery {
	return vectordb.FindSimilarQuery{
		CatalogID: "test-catalog",
		Model:     "test-model",
		Vector:    []float32{0.1, 0.2, 0.3},
		Limit:     5,
	}
}

var _ = Describe("VectorDB System", Ordered, func() {

	BeforeAll(func() {
		testspecs.LogTestProgress("Starting VectorDB BDD test suite")
	})

	AfterAll(func() {
		testspecs.LogTestProgress("VectorDB BDD test suite completed")
	})

	// =================================================================
	// LEVEL 1: BEHAVIORAL SPECIFICATIONS
	// These specs test the "why" - what business behaviors the vector
	// database supports and why tenant isolation matters for embeddings
	// =================================================================

	Describe("Embedding Storage Behaviors", func() {
		Context("when enforcing tenant isolation on vector operations", func() {
			It("rejects embedding storage without tenant context to prevent data leakage", func() {
				store := newTestStore()

				embedding := vectordb.Embedding{
					CatalogID: "nist-800-53",
					ControlID: "AC-1",
					Model:     "text-embedding-ada-002",
					Vector:    []float32{0.1, 0.2, 0.3},
				}

				By("blocking StoreEmbedding when no tenant is in context")
				err := store.StoreEmbedding(context.Background(), "test-tenant", embedding)
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(ContainSubstring("tenant")))

				By("blocking StoreBatch when no tenant is in context")
				err = store.StoreBatch(context.Background(), "test-tenant", []vectordb.Embedding{embedding})
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(ContainSubstring("tenant")))
			})

			It("rejects similarity searches without tenant context to prevent cross-tenant queries", func() {
				store := newTestStore()

				By("blocking FindSimilar when no tenant is in context")
				_, err := store.FindSimilar(context.Background(), "test-tenant", testFindSimilarQuery())
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(ContainSubstring("tenant")))
			})

			It("detects tenant mismatch between context and parameters to prevent spoofing", func() {
				store := newTestStore()

				embedding := vectordb.Embedding{
					CatalogID: "test-catalog",
					ControlID: "test-control",
					Model:     "test-model",
					Vector:    []float32{0.1, 0.2, 0.3},
				}

				By("rejecting StoreEmbedding when context tenant differs from parameter")
				err := store.StoreEmbedding(mismatchCtx(), "param-tenant", embedding)
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(ContainSubstring("mismatch")))

				By("rejecting StoreBatch when context tenant differs from parameter")
				err = store.StoreBatch(mismatchCtx(), "param-tenant", []vectordb.Embedding{embedding})
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(ContainSubstring("mismatch")))

				By("rejecting FindSimilar when context tenant differs from parameter")
				_, err = store.FindSimilar(mismatchCtx(), "param-tenant", testFindSimilarQuery())
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(ContainSubstring("mismatch")))

				By("rejecting DeleteByModel when context tenant differs from parameter")
				err = store.DeleteByModel(mismatchCtx(), "param-tenant", "test-catalog", "test-model")
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(ContainSubstring("mismatch")))
			})
		})

		Context("when validating query parameters for search safety", func() {
			It("rejects queries with invalid limits to prevent resource exhaustion", func() {
				store := newTestStore()
				ctx, err := tenant.WithTenant(context.Background(), "test-tenant")
				Expect(err).NotTo(HaveOccurred())

				By("rejecting zero limit")
				q := testFindSimilarQuery()
				q.Limit = 0
				_, err = store.FindSimilar(ctx, "test-tenant", q)
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(ContainSubstring("limit must be positive")))

				By("rejecting negative limit")
				q.Limit = -1
				_, err = store.FindSimilar(ctx, "test-tenant", q)
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(ContainSubstring("limit must be positive")))
			})

			It("rejects queries with empty vectors to prevent meaningless searches", func() {
				store := newTestStore()
				ctx, err := tenant.WithTenant(context.Background(), "test-tenant")
				Expect(err).NotTo(HaveOccurred())

				By("rejecting empty vector slice")
				q := testFindSimilarQuery()
				q.Vector = []float32{}
				_, err = store.FindSimilar(ctx, "test-tenant", q)
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(ContainSubstring("query vector cannot be empty")))

				By("rejecting nil vector")
				q.Vector = nil
				_, err = store.FindSimilar(ctx, "test-tenant", q)
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(ContainSubstring("query vector cannot be empty")))
			})
		})

		Context("when handling batch operations efficiently", func() {
			It("treats empty batch as a no-op without errors", func() {
				store := newTestStore()
				ctx, err := tenant.WithTenant(context.Background(), "test-tenant")
				Expect(err).NotTo(HaveOccurred())

				err = store.StoreBatch(ctx, "test-tenant", []vectordb.Embedding{})
				Expect(err).NotTo(HaveOccurred())
			})
		})
	})

	Describe("Domain Error Semantics", func() {
		Context("when communicating failure reasons to callers", func() {
			It("provides distinct error sentinels for model-related failures", func() {
				By("signalling when a query model is incompatible with stored embeddings")
				Expect(vectordb.ErrIncompatibleModel.Error()).To(
					Equal("query model does not match stored embeddings"))

				By("signalling when no embeddings exist for the specified model")
				Expect(vectordb.ErrModelNotFound.Error()).To(
					Equal("no embeddings found for specified model"))
			})
		})
	})

	// =================================================================
	// LEVEL 2: INTERFACE COMPLIANCE SPECIFICATIONS
	// These specs test the "how" - that PgVectorStore fulfills both the
	// Index and VectorDB interface contracts
	// =================================================================

	Describe("Interface Compliance", func() {
		Context("when verifying PgVectorStore implements required interfaces", func() {
			It("satisfies the VectorDB interface contract", func() {
				store := newTestStore()
				var _ vectordb.VectorDB = store
			})

			It("satisfies the Index interface contract", func() {
				store := newTestStore()
				var _ vectordb.Index = store
			})
		})

		Context("when verifying VectorDB interface can be implemented by any type", func() {
			It("allows custom implementations that satisfy the VectorDB contract", func() {
				// Compile-time check that testVectorDBImpl satisfies VectorDB
				var _ vectordb.VectorDB = (*testVectorDBImpl)(nil)
			})
		})

		Context("when enforcing tenant isolation on Index methods", func() {
			var store *vectordb.PgVectorStore

			BeforeEach(func() {
				store = newTestStore()
			})

			It("requires tenant context for Insert operations", func() {
				err := store.Insert(context.Background(), "ctrl-1", []float32{0.1, 0.2}, nil)
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(ContainSubstring("tenant")))
			})

			It("requires tenant context for Search operations", func() {
				_, err := store.Search(context.Background(), []float32{0.1, 0.2}, 5)
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(ContainSubstring("tenant")))
			})

			It("requires tenant context for Delete operations", func() {
				err := store.Delete(context.Background(), "ctrl-1")
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(ContainSubstring("tenant")))
			})

			It("requires tenant context for Get operations", func() {
				_, err := store.Get(context.Background(), "ctrl-1")
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(ContainSubstring("tenant")))
			})

			It("requires tenant context for Count operations", func() {
				_, err := store.Count(context.Background())
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(ContainSubstring("tenant")))
			})
		})
	})

	// =================================================================
	// LEVEL 3: TECHNICAL EDGE CASES AND INTEGRATION SCENARIOS
	// These specs test the "what" - comprehensive coverage of technical
	// scenarios from the original test files
	// =================================================================

	Describe("PgVectorStore Construction", func() {
		Context("when creating a store without telemetry", func() {
			It("initializes with nil telemetry fields", func() {
				store, err := vectordb.NewPgVectorStore(&sql.DB{})
				Expect(err).NotTo(HaveOccurred())
				Expect(store).NotTo(BeNil())

				tf := store.GetTelemetryFields()
				Expect(tf.HasTracer).To(BeFalse(), "tracer should be nil without telemetry")
				Expect(tf.HasMeter).To(BeFalse(), "meter should be nil without telemetry")
				Expect(tf.HasSearchCounter).To(BeFalse(), "searchCounter should be nil without telemetry")
				Expect(tf.HasSearchLatency).To(BeFalse(), "searchLatency should be nil without telemetry")
				Expect(tf.HasStoreCounter).To(BeFalse(), "storeCounter should be nil without telemetry")
				Expect(tf.HasStoreLatency).To(BeFalse(), "storeLatency should be nil without telemetry")
			})
		})

		Context("when creating a store with telemetry", func() {
			It("initializes all telemetry instruments", func() {
				tp := tracenoop.NewTracerProvider()
				tracer := tp.Tracer("vectordb-test")
				mp := metricnoop.NewMeterProvider()
				meter := mp.Meter("vectordb-test")

				store, err := vectordb.NewPgVectorStore(&sql.DB{}, vectordb.WithTelemetry(tracer, meter))
				Expect(err).NotTo(HaveOccurred())
				Expect(store).NotTo(BeNil())

				tf := store.GetTelemetryFields()
				Expect(tf.HasTracer).To(BeTrue(), "tracer should be set with telemetry")
				Expect(tf.HasMeter).To(BeTrue(), "meter should be set with telemetry")
				Expect(tf.HasSearchCounter).To(BeTrue(), "searchCounter should be set with telemetry")
				Expect(tf.HasSearchLatency).To(BeTrue(), "searchLatency should be set with telemetry")
				Expect(tf.HasStoreCounter).To(BeTrue(), "storeCounter should be set with telemetry")
				Expect(tf.HasStoreLatency).To(BeTrue(), "storeLatency should be set with telemetry")
			})
		})

		Context("when given a nil database connection", func() {
			It("returns an error regardless of other options", func() {
				By("failing with no options")
				_, err := vectordb.NewPgVectorStore(nil)
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(ContainSubstring("database connection is required")))

				By("failing even when telemetry options are provided")
				tp := tracenoop.NewTracerProvider()
				tracer := tp.Tracer("vectordb-test")
				mp := metricnoop.NewMeterProvider()
				meter := mp.Meter("vectordb-test")

				_, err = vectordb.NewPgVectorStore(nil, vectordb.WithTelemetry(tracer, meter))
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(ContainSubstring("database connection is required")))
			})
		})
	})

	Describe("Embedding Type Validation", func() {
		Context("when constructing Embedding structs", func() {
			It("holds compliance-specific metadata fields correctly", func() {
				embedding := vectordb.Embedding{
					CatalogID: "nist-800-53",
					ControlID: "AC-1",
					Model:     "text-embedding-ada-002",
					Vector:    []float32{0.1, 0.2, 0.3},
					Metadata: map[string]any{
						"oscal.type":   "control",
						"oscal.family": "AC",
					},
				}

				Expect(embedding.CatalogID).NotTo(BeEmpty())
				Expect(embedding.ControlID).To(Equal("AC-1"))
				Expect(embedding.Model).To(Equal("text-embedding-ada-002"))
				Expect(embedding.Vector).To(HaveLen(3))
				Expect(embedding.Metadata).To(HaveKey("oscal.type"))
				Expect(embedding.Metadata).To(HaveKey("oscal.family"))
			})
		})

		Context("when constructing FindSimilarQuery structs", func() {
			It("holds all required search parameters", func() {
				query := vectordb.FindSimilarQuery{
					CatalogID: "nist-800-53",
					Model:     "text-embedding-ada-002",
					Vector:    []float32{0.1, 0.2},
					Limit:     10,
				}

				Expect(query.CatalogID).NotTo(BeEmpty())
				Expect(query.Model).NotTo(BeEmpty())
				Expect(query.Vector).NotTo(BeEmpty())
				Expect(query.Limit).To(BeNumerically(">", 0))
			})
		})
	})

	Describe("Vector String Parsing", func() {
		Context("when parsing valid pgvector format strings", func() {
			It("parses standard floating-point vectors", func() {
				result, err := vectordb.ParseVectorString("[1.0,2.0,3.0]")
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal([]float32{1.0, 2.0, 3.0}))
			})

			It("parses single-element vectors", func() {
				result, err := vectordb.ParseVectorString("[0.5]")
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal([]float32{0.5}))
			})

			It("parses negative values", func() {
				result, err := vectordb.ParseVectorString("[-1.5,2.5,-3.5]")
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal([]float32{-1.5, 2.5, -3.5}))
			})

			It("parses integer values as float32", func() {
				result, err := vectordb.ParseVectorString("[1,2,3]")
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal([]float32{1, 2, 3}))
			})

			It("parses scientific notation", func() {
				result, err := vectordb.ParseVectorString("[1e-5,2.5e3]")
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal([]float32{1e-5, 2.5e3}))
			})

			It("parses empty brackets as nil slice", func() {
				result, err := vectordb.ParseVectorString("[]")
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(BeNil())
			})
		})

		Context("when parsing invalid pgvector format strings", func() {
			It("rejects strings without brackets", func() {
				_, err := vectordb.ParseVectorString("1.0,2.0")
				Expect(err).To(HaveOccurred())
			})

			It("rejects strings with non-numeric elements", func() {
				_, err := vectordb.ParseVectorString("[1.0,abc,3.0]")
				Expect(err).To(HaveOccurred())
			})

			It("rejects empty strings", func() {
				_, err := vectordb.ParseVectorString("")
				Expect(err).To(HaveOccurred())
			})
		})
	})

	Describe("DeleteByModel Tenant Enforcement", func() {
		Context("when deleting embeddings by model", func() {
			It("rejects deletion without tenant context", func() {
				store := newTestStore()

				err := store.DeleteByModel(context.Background(), "test-tenant", "test-catalog", "test-model")
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(ContainSubstring("tenant")))
			})

			It("rejects deletion with mismatched tenant context", func() {
				store := newTestStore()

				err := store.DeleteByModel(mismatchCtx(), "param-tenant", "test-catalog", "test-model")
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(ContainSubstring("mismatch")))
			})
		})
	})
})

// testVectorDBImpl is a minimal VectorDB implementation for compile-time
// interface verification, ported from interface_test.go.
type testVectorDBImpl struct{}

func (db *testVectorDBImpl) StoreEmbedding(_ context.Context, _ string, _ vectordb.Embedding) error {
	return nil
}

func (db *testVectorDBImpl) StoreBatch(_ context.Context, _ string, _ []vectordb.Embedding) error {
	return nil
}

func (db *testVectorDBImpl) FindSimilar(_ context.Context, _ string, _ vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
	return nil, nil
}

func (db *testVectorDBImpl) DeleteByModel(_ context.Context, _, _, _ string) error {
	return nil
}

var _ = Describe("Telemetry Wiring", func() {
	Context("when PgVectorStore is created without telemetry", func() {
		It("has nil telemetry fields", func() {
			store := newTestStore()
			tf := store.GetTelemetryFields()
			Expect(tf.HasTracer).To(BeFalse(), "tracer should be nil without telemetry")
			Expect(tf.HasMeter).To(BeFalse(), "meter should be nil without telemetry")
			Expect(tf.HasSearchCounter).To(BeFalse(), "searchCounter should be nil without telemetry")
			Expect(tf.HasSearchLatency).To(BeFalse(), "searchLatency should be nil without telemetry")
			Expect(tf.HasStoreCounter).To(BeFalse(), "storeCounter should be nil without telemetry")
			Expect(tf.HasStoreLatency).To(BeFalse(), "storeLatency should be nil without telemetry")
		})
	})

	Context("when PgVectorStore is created with telemetry", func() {
		It("populates all telemetry fields", func() {
			tp := tracenoop.NewTracerProvider()
			tracer := tp.Tracer("vectordb-test")
			mp := metricnoop.NewMeterProvider()
			meter := mp.Meter("vectordb-test")

			store := newTestStore(vectordb.WithTelemetry(tracer, meter))
			tf := store.GetTelemetryFields()
			Expect(tf.HasTracer).To(BeTrue(), "tracer should be set with telemetry")
			Expect(tf.HasMeter).To(BeTrue(), "meter should be set with telemetry")
			Expect(tf.HasSearchCounter).To(BeTrue(), "searchCounter should be set with telemetry")
			Expect(tf.HasSearchLatency).To(BeTrue(), "searchLatency should be set with telemetry")
			Expect(tf.HasStoreCounter).To(BeTrue(), "storeCounter should be set with telemetry")
			Expect(tf.HasStoreLatency).To(BeTrue(), "storeLatency should be set with telemetry")
		})
	})

	Context("when WithTelemetry option is provided", func() {
		It("creates a non-nil option function", func() {
			tp := tracenoop.NewTracerProvider()
			tracer := tp.Tracer("vectordb-test")
			mp := metricnoop.NewMeterProvider()
			meter := mp.Meter("vectordb-test")

			opt := vectordb.WithTelemetry(tracer, meter)
			Expect(opt).NotTo(BeNil())
		})
	})
})
