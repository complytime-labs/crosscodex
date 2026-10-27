package vectordbtest_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
	"github.com/complytime-labs/crosscodex/pkg/vectordb/vectordbtest"
)

func TestVectorDBTest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "VectorDBTest Suite")
}

var errScripted = errors.New("scripted")

var _ = Describe("Fake", func() {
	var (
		ctx  context.Context
		fake *vectordbtest.Fake
		emb  vectordb.Embedding
	)

	BeforeEach(func() {
		var err error
		ctx, err = tenant.WithTenant(context.Background(), "tenant-a")
		Expect(err).NotTo(HaveOccurred())
		fake = &vectordbtest.Fake{}
		emb = vectordb.Embedding{
			CatalogID: "cat", ControlID: "AC-1", Model: "m",
			Vector: []float32{0.1, 0.2}, Metadata: map[string]any{"k": "v"},
		}
	})

	Describe("tenant checks mirroring PgVectorStore", func() {
		// scriptAll scripts every method so a call that wrongly passed the
		// check would flip reached.
		scriptAll := func(f *vectordbtest.Fake) *bool {
			reached := false
			f.StoreEmbeddingFunc = func(context.Context, string, vectordb.Embedding) error { reached = true; return nil }
			f.StoreBatchFunc = func(context.Context, string, []vectordb.Embedding) error { reached = true; return nil }
			f.FindSimilarFunc = func(context.Context, string, vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
				reached = true
				return nil, nil
			}
			f.DeleteByModelFunc = func(context.Context, string, string, string) error { reached = true; return nil }
			return &reached
		}
		query := vectordb.FindSimilarQuery{CatalogID: "cat", Model: "m", Vector: []float32{1}, Limit: 1}

		DescribeTable("rejects a context without a tenant, recording and scripting nothing",
			func(call func(ctx context.Context, f *vectordbtest.Fake, tenantID string) error) {
				reached := scriptAll(fake)
				err := call(context.Background(), fake, "tenant-a")
				Expect(err).To(MatchError(tenant.ErrNoTenant))
				Expect(err).To(MatchError(ContainSubstring("tenant context required")))
				Expect(*reached).To(BeFalse())
				Expect(fake.Stored()).To(BeEmpty())
			},
			Entry("StoreEmbedding", func(ctx context.Context, f *vectordbtest.Fake, t string) error {
				return f.StoreEmbedding(ctx, t, vectordb.Embedding{})
			}),
			Entry("StoreBatch", func(ctx context.Context, f *vectordbtest.Fake, t string) error {
				return f.StoreBatch(ctx, t, []vectordb.Embedding{{}})
			}),
			Entry("FindSimilar", func(ctx context.Context, f *vectordbtest.Fake, t string) error {
				_, err := f.FindSimilar(ctx, t, query)
				return err
			}),
			Entry("DeleteByModel", func(ctx context.Context, f *vectordbtest.Fake, t string) error {
				return f.DeleteByModel(ctx, t, "cat", "m")
			}),
		)

		DescribeTable("rejects a tenant argument that differs from the context tenant",
			func(call func(ctx context.Context, f *vectordbtest.Fake, tenantID string) error) {
				reached := scriptAll(fake)
				err := call(ctx, fake, "tenant-b")
				Expect(err).To(MatchError(tenant.ErrTenantMismatch))
				Expect(err).To(MatchError(ContainSubstring("context=tenant-a, param=tenant-b")))
				Expect(*reached).To(BeFalse())
				Expect(fake.Stored()).To(BeEmpty())
			},
			Entry("StoreEmbedding", func(ctx context.Context, f *vectordbtest.Fake, t string) error {
				return f.StoreEmbedding(ctx, t, vectordb.Embedding{})
			}),
			Entry("StoreBatch", func(ctx context.Context, f *vectordbtest.Fake, t string) error {
				return f.StoreBatch(ctx, t, []vectordb.Embedding{{}})
			}),
			Entry("FindSimilar", func(ctx context.Context, f *vectordbtest.Fake, t string) error {
				_, err := f.FindSimilar(ctx, t, query)
				return err
			}),
			Entry("DeleteByModel", func(ctx context.Context, f *vectordbtest.Fake, t string) error {
				return f.DeleteByModel(ctx, t, "cat", "m")
			}),
		)
	})

	Describe("FindSimilar query checks mirroring PgVectorStore", func() {
		DescribeTable("rejects an invalid query before the scripted Func",
			func(q vectordb.FindSimilarQuery, wantMsg string) {
				reached := false
				fake.FindSimilarFunc = func(context.Context, string, vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
					reached = true
					return nil, nil
				}
				_, err := fake.FindSimilar(ctx, "tenant-a", q)
				Expect(err).To(MatchError(ContainSubstring(wantMsg)))
				Expect(reached).To(BeFalse())
			},
			Entry("zero limit", vectordb.FindSimilarQuery{Vector: []float32{1}, Limit: 0}, "limit must be positive, got 0"),
			Entry("negative limit", vectordb.FindSimilarQuery{Vector: []float32{1}, Limit: -3}, "limit must be positive, got -3"),
			Entry("nil vector", vectordb.FindSimilarQuery{Limit: 1}, "query vector cannot be empty"),
			Entry("empty vector", vectordb.FindSimilarQuery{Vector: []float32{}, Limit: 1}, "query vector cannot be empty"),
		)

		It("does not require a catalog or model, as production does not check them before SQL", func() {
			fake.FindSimilarFunc = func(context.Context, string, vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
				return nil, nil
			}
			_, err := fake.FindSimilar(ctx, "tenant-a", vectordb.FindSimilarQuery{Vector: []float32{1}, Limit: 1})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("unscripted calls", func() {
		It("StoreEmbedding succeeds and records the call", func() {
			Expect(fake.StoreEmbedding(ctx, "tenant-a", emb)).To(Succeed())
			Expect(fake.Stored()).To(Equal([]vectordbtest.StoreCall{{Tenant: "tenant-a", Embeddings: []vectordb.Embedding{emb}}}))
		})

		It("StoreBatch succeeds and records the call", func() {
			Expect(fake.StoreBatch(ctx, "tenant-a", []vectordb.Embedding{emb, emb})).To(Succeed())
			Expect(fake.Stored()).To(Equal([]vectordbtest.StoreCall{{Tenant: "tenant-a", Embeddings: []vectordb.Embedding{emb, emb}}}))
		})

		It("StoreBatch accepts and records an empty batch", func() {
			Expect(fake.StoreBatch(ctx, "tenant-a", nil)).To(Succeed())
			Expect(fake.StoreBatch(ctx, "tenant-a", []vectordb.Embedding{})).To(Succeed())
			stored := fake.Stored()
			Expect(stored).To(HaveLen(2))
			Expect(stored[0].Embeddings).To(BeEmpty())
			Expect(stored[1].Embeddings).To(BeEmpty())
		})

		It("FindSimilar returns ErrUnscripted naming the Func", func() {
			res, err := fake.FindSimilar(ctx, "tenant-a", vectordb.FindSimilarQuery{Vector: []float32{1}, Limit: 1})
			Expect(res).To(BeNil())
			Expect(err).To(MatchError(vectordbtest.ErrUnscripted))
			Expect(err).To(MatchError(ContainSubstring("Fake.FindSimilarFunc is not set")))
		})

		It("DeleteByModel returns ErrUnscripted naming the Func", func() {
			err := fake.DeleteByModel(ctx, "tenant-a", "cat", "m")
			Expect(err).To(MatchError(vectordbtest.ErrUnscripted))
			Expect(err).To(MatchError(ContainSubstring("Fake.DeleteByModelFunc is not set")))
		})
	})

	Describe("scripted calls", func() {
		It("lets a store Func alone decide the result and still records the call", func() {
			fake.StoreEmbeddingFunc = func(context.Context, string, vectordb.Embedding) error { return errScripted }
			fake.StoreBatchFunc = func(context.Context, string, []vectordb.Embedding) error { return errScripted }
			Expect(fake.StoreEmbedding(ctx, "tenant-a", emb)).To(MatchError(errScripted))
			Expect(fake.StoreBatch(ctx, "tenant-a", []vectordb.Embedding{emb})).To(MatchError(errScripted))
			Expect(fake.Stored()).To(HaveLen(2))
		})

		It("hands FindSimilarFunc and DeleteByModelFunc the caller's arguments", func() {
			want := []vectordb.SimilarityResult{{ControlID: "AC-2", Similarity: 0.9}}
			q := vectordb.FindSimilarQuery{CatalogID: "cat", Model: "m", Vector: []float32{1, 2}, Limit: 5}
			fake.FindSimilarFunc = func(_ context.Context, tenantID string, got vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
				Expect(tenantID).To(Equal("tenant-a"))
				Expect(got).To(Equal(q))
				return want, nil
			}
			fake.DeleteByModelFunc = func(_ context.Context, tenantID, catalogID, model string) error {
				Expect([]string{tenantID, catalogID, model}).To(Equal([]string{"tenant-a", "cat", "m"}))
				return errScripted
			}
			got, err := fake.FindSimilar(ctx, "tenant-a", q)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
			Expect(fake.DeleteByModel(ctx, "tenant-a", "cat", "m")).To(MatchError(errScripted))
		})

		It("returns scripted sentinel errors intact", func() {
			fake.FindSimilarFunc = func(context.Context, string, vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
				return nil, vectordb.ErrModelNotFound
			}
			_, err := fake.FindSimilar(ctx, "tenant-a", vectordb.FindSimilarQuery{Vector: []float32{1}, Limit: 1})
			Expect(err).To(MatchError(vectordb.ErrModelNotFound))
		})
	})

	Describe("Stored recorder", func() {
		It("is unaffected by the caller mutating its batch after the call", func() {
			batch := []vectordb.Embedding{emb}
			batch[0].Vector = []float32{0.1, 0.2}
			batch[0].Metadata = map[string]any{"k": "v"}
			Expect(fake.StoreBatch(ctx, "tenant-a", batch)).To(Succeed())
			batch[0].Vector[0] = 9
			batch[0].Metadata["k"] = "mutated"
			batch[0].ControlID = "mutated"
			Expect(fake.Stored()[0].Embeddings[0]).To(Equal(vectordb.Embedding{
				CatalogID: "cat", ControlID: "AC-1", Model: "m",
				Vector: []float32{0.1, 0.2}, Metadata: map[string]any{"k": "v"},
			}))
		})

		It("returns copies, so editing the result leaves the record intact", func() {
			Expect(fake.StoreEmbedding(ctx, "tenant-a", emb)).To(Succeed())
			got := fake.Stored()
			got[0].Tenant = "mutated"
			got[0].Embeddings[0].Vector[0] = 9
			got[0].Embeddings[0].Metadata["k"] = "mutated"
			again := fake.Stored()
			Expect(again[0].Tenant).To(Equal("tenant-a"))
			Expect(again[0].Embeddings[0].Vector).To(Equal([]float32{0.1, 0.2}))
			Expect(again[0].Embeddings[0].Metadata).To(Equal(map[string]any{"k": "v"}))
		})

		It("does not record FindSimilar or DeleteByModel calls", func() {
			fake.FindSimilarFunc = func(context.Context, string, vectordb.FindSimilarQuery) ([]vectordb.SimilarityResult, error) {
				return nil, nil
			}
			fake.DeleteByModelFunc = func(context.Context, string, string, string) error { return nil }
			_, _ = fake.FindSimilar(ctx, "tenant-a", vectordb.FindSimilarQuery{Vector: []float32{1}, Limit: 1})
			Expect(fake.DeleteByModel(ctx, "tenant-a", "cat", "m")).To(Succeed())
			Expect(fake.Stored()).To(BeEmpty())
		})
	})

	Describe("concurrency", func() {
		It("records every call from 20 goroutines", func() {
			var wg sync.WaitGroup
			for range 20 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer GinkgoRecover()
					Expect(fake.StoreBatch(ctx, "tenant-a", []vectordb.Embedding{emb})).To(Succeed())
					_ = fake.Stored()
				}()
			}
			wg.Wait()
			Expect(fake.Stored()).To(HaveLen(20))
		})
	})
})
