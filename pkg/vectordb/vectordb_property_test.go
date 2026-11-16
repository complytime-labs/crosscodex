package vectordb_test

import (
	"context"
	"database/sql"
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
)

// Suite bootstrap lives in vectordb_bdd_test.go — do NOT add RunSpecs here.

var _ = Describe("Property Specifications", Ordered, func() {
	Context("vectorToString / parseVectorString — roundtrip", func() {
		It("roundtrip preserves float32 values within precision", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				n := rapid.IntRange(1, 100).Draw(t, "n")
				vec := make([]float32, n)
				for i := range vec {
					vec[i] = rapid.Float32Range(-1e6, 1e6).Draw(t, "elem")
				}
				str := vectordb.VectorToString(vec)
				parsed, err := vectordb.ParseVectorString(str)
				if err != nil {
					t.Fatalf("parseVectorString failed: %v for input %q", err, str)
				}
				if len(parsed) != len(vec) {
					t.Fatalf("length mismatch: %d vs %d", len(vec), len(parsed))
				}
				for i := range vec {
					if math.Abs(float64(vec[i]-parsed[i])) > 1e-6 {
						t.Fatalf("element %d mismatch: %v vs %v", i, vec[i], parsed[i])
					}
				}
			})
		})
	})

	Context("parseVectorString — basic cases", func() {
		It("parses valid vector string", func() {
			parsed, err := vectordb.ParseVectorString("[1.0,2.0,3.0]")
			Expect(err).NotTo(HaveOccurred())
			Expect(parsed).To(HaveLen(3))
		})

		It("returns nil for empty brackets", func() {
			parsed, err := vectordb.ParseVectorString("[]")
			Expect(err).NotTo(HaveOccurred())
			Expect(parsed).To(BeNil())
		})
	})

	Context("GetEmbeddings — tenant validation", func() {
		It("a context tenant differing from the argument always errors before touching the DB", func() {
			// &sql.DB{} has no driver: reaching it would panic, not return an error.
			store, err := vectordb.NewPgVectorStore(&sql.DB{})
			Expect(err).NotTo(HaveOccurred())
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				ctxTenant := rapid.StringMatching(`^[a-z][a-z0-9-]{2,12}$`).Draw(t, "ctxTenant")
				param := rapid.StringMatching(`^[a-z][a-z0-9-]{2,12}$`).Filter(func(s string) bool { return s != ctxTenant }).Draw(t, "param")
				ctx, err := tenant.WithTenant(context.Background(), ctxTenant)
				if err != nil {
					t.Skip("generated tenant ID rejected by tenant.WithTenant")
				}
				got, err := store.GetEmbeddings(ctx, param, rapid.String().Draw(t, "catalog"), rapid.String().Draw(t, "control"))
				if err == nil || got != nil {
					t.Fatalf("GetEmbeddings(ctx tenant %q, param %q) = (%v, %v), want nil and a mismatch error", ctxTenant, param, got, err)
				}
			})
		})
	})
})
