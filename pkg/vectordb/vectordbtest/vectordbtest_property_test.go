package vectordbtest_test

import (
	"context"
	"slices"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
	"github.com/complytime-labs/crosscodex/pkg/vectordb/vectordbtest"
)

func embeddingGen() *rapid.Generator[vectordb.Embedding] {
	return rapid.Custom(func(t *rapid.T) vectordb.Embedding {
		return vectordb.Embedding{
			CatalogID: rapid.String().Draw(t, "catalog"),
			ControlID: rapid.String().Draw(t, "control"),
			Model:     rapid.String().Draw(t, "model"),
			Vector:    rapid.SliceOf(rapid.Float32()).Draw(t, "vector"),
		}
	})
}

var _ = Describe("Property Specifications", Ordered, func() {
	It("Stored preserves call order and content for any mix of store calls", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			ctx, err := tenant.WithTenant(context.Background(), "tenant-p")
			if err != nil {
				t.Fatalf("WithTenant: %v", err)
			}
			fake := &vectordbtest.Fake{}
			batches := rapid.SliceOfN(rapid.SliceOfN(embeddingGen(), 0, 4), 0, 6).Draw(t, "batches")
			single := rapid.SliceOfN(rapid.Bool(), len(batches), len(batches)).Draw(t, "single")

			for i, batch := range batches {
				if single[i] && len(batch) > 0 {
					err = fake.StoreEmbedding(ctx, "tenant-p", batch[0])
					batches[i] = batch[:1]
				} else {
					err = fake.StoreBatch(ctx, "tenant-p", batch)
				}
				if err != nil {
					t.Fatalf("store call %d: %v", i, err)
				}
			}

			got := fake.Stored()
			if len(got) != len(batches) {
				t.Fatalf("Stored() has %d calls, want %d", len(got), len(batches))
			}
			for i, want := range batches {
				if got[i].Tenant != "tenant-p" || len(got[i].Embeddings) != len(want) {
					t.Fatalf("Stored()[%d] = %+v, want %d embeddings for tenant-p", i, got[i], len(want))
				}
				for j, e := range want {
					g := got[i].Embeddings[j]
					if g.CatalogID != e.CatalogID || g.ControlID != e.ControlID || g.Model != e.Model || !slices.Equal(g.Vector, e.Vector) {
						t.Fatalf("Stored()[%d].Embeddings[%d] = %+v, want %+v", i, j, g, e)
					}
				}
			}
		})
	})

	It("a tenant argument differing from the context tenant is always rejected and never recorded", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			ctxTenant := rapid.StringMatching(`^[a-z][a-z0-9-]{2,12}$`).Draw(t, "ctxTenant")
			param := rapid.StringMatching(`^[a-z][a-z0-9-]{2,12}$`).Filter(func(s string) bool { return s != ctxTenant }).Draw(t, "param")
			ctx, err := tenant.WithTenant(context.Background(), ctxTenant)
			if err != nil {
				t.Skip("generated tenant ID rejected by tenant.WithTenant")
			}
			fake := &vectordbtest.Fake{}
			if err := fake.StoreBatch(ctx, param, []vectordb.Embedding{{}}); err == nil {
				t.Fatalf("StoreBatch(ctx tenant %q, param %q) = nil, want mismatch error", ctxTenant, param)
			}
			if n := len(fake.Stored()); n != 0 {
				t.Fatalf("rejected call was recorded: %d entries", n)
			}
		})
	})
})
