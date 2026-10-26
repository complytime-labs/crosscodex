package results_test

import (
	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/internal/results"
)

var _ = Describe("Property Specifications", Ordered, func() {
	It("round-trips any cursor", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			c := results.Cursor{
				JobID:    rapid.StringN(0, 256, -1).Draw(t, "job"),
				SourceID: rapid.StringN(0, 256, -1).Draw(t, "src"),
				TargetID: rapid.StringN(0, 256, -1).Draw(t, "tgt"),
			}
			got, err := results.DecodeCursor(results.EncodeCursor(c))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if *got != c {
				t.Fatalf("got %+v want %+v", *got, c)
			}
		})
	})
})
