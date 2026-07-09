package analysis_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/internal/analysis"
)

var _ = Describe("Property Specifications", Ordered, func() {
	Context("computeBackoff — bounded exponential with jitter", func() {
		It("never exceeds base * 2^attempt * 1.25", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				baseMs := rapid.IntRange(100, 5000).Draw(t, "baseMs")
				attempt := rapid.IntRange(0, 10).Draw(t, "attempt")
				base := time.Duration(baseMs) * time.Millisecond

				result := analysis.ExportComputeBackoff(attempt, base)

				maxExpected := time.Duration(float64(base) * float64(int(1)<<uint(attempt)) * 1.25)
				if result > maxExpected {
					t.Fatalf("backoff %v exceeded max %v for base=%v attempt=%d", result, maxExpected, base, attempt)
				}
			})
		})

		It("never goes below base * 2^attempt * 0.75", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				baseMs := rapid.IntRange(100, 5000).Draw(t, "baseMs")
				attempt := rapid.IntRange(0, 10).Draw(t, "attempt")
				base := time.Duration(baseMs) * time.Millisecond

				result := analysis.ExportComputeBackoff(attempt, base)

				minExpected := time.Duration(float64(base) * float64(int(1)<<uint(attempt)) * 0.75)
				if result < minExpected {
					t.Fatalf("backoff %v below min %v for base=%v attempt=%d", result, minExpected, base, attempt)
				}
			})
		})

		It("is always non-negative", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				baseMs := rapid.IntRange(0, 5000).Draw(t, "baseMs")
				attempt := rapid.IntRange(0, 10).Draw(t, "attempt")
				base := time.Duration(baseMs) * time.Millisecond

				result := analysis.ExportComputeBackoff(attempt, base)
				if result < 0 {
					t.Fatalf("negative backoff %v for base=%v attempt=%d", result, base, attempt)
				}
			})
		})
	})
})
