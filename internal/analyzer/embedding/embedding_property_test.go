package embedding_test

import (
	"fmt"
	"math"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/internal/analyzer/embedding"
)

var _ = Describe("Property Specifications", Ordered, func() {
	Context("cosineSimilarity — symmetry", func() {
		It("cosineSimilarity(a, b) == cosineSimilarity(b, a)", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				dim := rapid.IntRange(1, 100).Draw(t, "dim")
				a := drawFloat32Slice(t, dim, "a")
				b := drawFloat32Slice(t, dim, "b")
				ab := embedding.ExportCosineSimilarity(a, b)
				ba := embedding.ExportCosineSimilarity(b, a)
				if diff := math.Abs(float64(ab - ba)); diff > 1e-6 {
					t.Fatalf("asymmetric: sim(a,b)=%g != sim(b,a)=%g (diff=%g)", ab, ba, diff)
				}
			})
		})
	})

	Context("cosineSimilarity — self-similarity", func() {
		It("returns 1.0 for non-zero vectors", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				dim := rapid.IntRange(1, 100).Draw(t, "dim")
				v := drawFloat32Slice(t, dim, "v")
				// Ensure at least one non-zero element.
				v[0] = rapid.Float32Range(0.01, 10.0).Draw(t, "nonzero")
				sim := embedding.ExportCosineSimilarity(v, v)
				if diff := math.Abs(float64(sim) - 1.0); diff > 1e-5 {
					t.Fatalf("self-similarity %g != 1.0 for non-zero vector", sim)
				}
			})
		})
	})

	Context("cosineSimilarity — range", func() {
		It("result is always in [-1.0, 1.0]", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				dim := rapid.IntRange(1, 100).Draw(t, "dim")
				a := drawFloat32Slice(t, dim, "a")
				b := drawFloat32Slice(t, dim, "b")
				sim := embedding.ExportCosineSimilarity(a, b)
				if sim < -1.0-1e-6 || sim > 1.0+1e-6 {
					t.Fatalf("out of range: %g", sim)
				}
			})
		})
	})

	Context("cosineSimilarity — zero safety", func() {
		It("returns 0.0 for zero vectors", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				dim := rapid.IntRange(1, 100).Draw(t, "dim")
				zero := make([]float32, dim)
				other := drawFloat32Slice(t, dim, "other")
				sim := embedding.ExportCosineSimilarity(zero, other)
				if sim != 0.0 {
					t.Fatalf("zero vector similarity %g != 0.0", sim)
				}
			})
		})
	})

	Context("cleanForEmbedding — idempotency", func() {
		It("cleanForEmbedding(cleanForEmbedding(x)) == cleanForEmbedding(x)", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				text := rapid.String().Draw(t, "text")
				once := embedding.ExportCleanForEmbedding(text)
				twice := embedding.ExportCleanForEmbedding(once)
				if once != twice {
					t.Fatalf("not idempotent: %q -> %q -> %q", text, once, twice)
				}
			})
		})
	})

	Context("prepareText — truncation bound", func() {
		It("runeCount(prepareText(text, '', n)) <= n for all n > 0", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				text := rapid.String().Draw(t, "text")
				maxChars := rapid.IntRange(1, 5000).Draw(t, "maxChars")
				result := embedding.ExportPrepareText(text, "", maxChars)
				runeCount := utf8.RuneCountInString(result)
				if runeCount > maxChars {
					t.Fatalf("prepareText output %d runes exceeds maxChars %d", runeCount, maxChars)
				}
			})
		})
	})

	Context("buildSimilarityMatrix — symmetry", func() {
		It("matrix[i][j] == matrix[j][i] for same-set embeddings", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				n := rapid.IntRange(2, 10).Draw(t, "n")
				dim := rapid.IntRange(2, 20).Draw(t, "dim")
				embeddings := make(map[string][]float32, n)
				ids := make([]string, n)
				for i := range n {
					id := fmt.Sprintf("ctrl-%d", i)
					ids[i] = id
					vec := drawFloat32Slice(t, dim, "vec")
					// Ensure non-zero
					vec[0] = rapid.Float32Range(0.01, 10.0).Draw(t, "nonzero")
					embeddings[id] = vec
				}
				matrix := embedding.ExportBuildSimilarityMatrix(embeddings, ids)
				for i := range ids {
					for j := range ids {
						diff := math.Abs(float64(matrix.Values[i][j] - matrix.Values[j][i]))
						if diff > 0.01 {
							t.Fatalf("asymmetric matrix: [%d][%d]=%g != [%d][%d]=%g",
								i, j, matrix.Values[i][j], j, i, matrix.Values[j][i])
						}
					}
				}
			})
		})
	})
})

// drawFloat32Slice generates a []float32 slice of the given dimension.
func drawFloat32Slice(t *rapid.T, dim int, label string) []float32 {
	v := make([]float32, dim)
	for i := range dim {
		v[i] = rapid.Float32Range(-100.0, 100.0).Draw(t, label)
	}
	return v
}
