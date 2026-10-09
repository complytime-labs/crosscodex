package artifacts_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
)

var _ = Describe("Property Specifications", func() {
	noSameLine := rapid.String().Filter(func(s string) bool {
		return !strings.Contains(strings.ToUpper(s), "SAME:")
	})

	It("never panics and yields no decision without a SAME: line", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			raw := noSameLine.Draw(t, "raw")
			if d := artifacts.ParseSameAsResponse("v", raw).Decision; d != nil {
				t.Fatalf("decision %v from %q, which has no SAME: line", *d, raw)
			}
		})
	})

	It("reads the decision of a SAME: line placed anywhere", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			prefix := noSameLine.Draw(t, "prefix")
			suffix := rapid.String().Draw(t, "suffix")
			word := rapid.SampledFrom([]string{"YES", "yes", "Yes", "NO", "no", "No"}).Draw(t, "word")
			// The decision line ends at the newline before suffix, so suffix
			// cannot extend it.
			raw := prefix + "\nSAME: " + word + "\n" + suffix
			d := artifacts.ParseSameAsResponse("v", raw).Decision
			if want := strings.EqualFold(word, "yes"); d == nil || *d != want {
				t.Fatalf("decision %v from %q, want %v", d, raw, want)
			}
		})
	})
})
