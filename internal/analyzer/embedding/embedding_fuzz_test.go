package embedding_test

import (
	"math"
	"testing"

	"github.com/complytime-labs/crosscodex/internal/analyzer/embedding"
)

func FuzzCleanForEmbedding(f *testing.F) {
	f.Add("")
	f.Add("clean text")
	f.Add("{{ insert: param, ac-1_prm_1 }}")
	f.Add("VerDate Sep 11 2014 10:47 Jan 20, 2023")
	f.Add("Jkt 099006 PO 00000")
	f.Add("G:\\COMP\\PUBL\\Title47.xml")
	f.Add("|---|---|")
	f.Add("\n\n\n\n\n")
	f.Add("   \t\t   ")
	f.Add("\x00\xff\xfe")

	f.Fuzz(func(t *testing.T, raw string) {
		// Must not panic on any input.
		_ = embedding.ExportCleanForEmbedding(raw)
	})
}

func FuzzPrepareText(f *testing.F) {
	f.Add("statement", "ancestor", 1500)
	f.Add("", "", 0)
	f.Add("text", "", -1)
	f.Add("{{ insert: param }}", "ROOT", 10)
	f.Add("\x00\xff", "\x00", 5)

	f.Fuzz(func(t *testing.T, statement, ancestor string, maxChars int) {
		// Must not panic on any input.
		_ = embedding.ExportPrepareText(statement, ancestor, maxChars)
	})
}

func FuzzCosineSimilarity(f *testing.F) {
	// Seed with known 3-dim vector pairs.
	f.Add(float32(1.0), float32(2.0), float32(3.0), float32(4.0), float32(5.0), float32(6.0))
	f.Add(float32(0.0), float32(0.0), float32(0.0), float32(1.0), float32(2.0), float32(3.0))
	f.Add(float32(1.0), float32(0.0), float32(0.0), float32(-1.0), float32(0.0), float32(0.0))

	f.Fuzz(func(t *testing.T, a1, a2, a3, b1, b2, b3 float32) {
		// Skip NaN/Inf inputs — these are not valid embeddings.
		for _, v := range []float32{a1, a2, a3, b1, b2, b3} {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				return
			}
		}
		a := []float32{a1, a2, a3}
		b := []float32{b1, b2, b3}
		result := embedding.ExportCosineSimilarity(a, b)

		// Result must not be NaN or Inf.
		if math.IsNaN(float64(result)) {
			t.Fatalf("cosineSimilarity returned NaN for a=%v b=%v", a, b)
		}
		if math.IsInf(float64(result), 0) {
			t.Fatalf("cosineSimilarity returned Inf for a=%v b=%v", a, b)
		}
		if result < -1.0-1e-5 || result > 1.0+1e-5 {
			t.Fatalf("cosineSimilarity out of range: %g for a=%v b=%v", result, a, b)
		}
	})
}
