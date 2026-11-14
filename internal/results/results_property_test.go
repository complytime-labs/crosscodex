package results_test

import (
	"encoding/json"
	"reflect"

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

	It("round-trips any valid contribution list", func() {
		contribution := rapid.Custom(func(t *rapid.T) results.Contribution {
			return results.Contribution{
				Generator: rapid.StringN(1, 64, -1).Draw(t, "generator"),
				Score:     rapid.Float64Range(-1e6, 1e6).Draw(t, "score"),
				Weight:    rapid.Float64Range(-1e6, 1e6).Draw(t, "weight"),
				Metadata:  rapid.MapOfN(rapid.StringN(0, 16, -1), rapid.StringN(0, 16, -1), 1, 4).Draw(t, "metadata"),
			}
		})
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			want := rapid.SliceOf(contribution).Draw(t, "contributions")
			// The writer's wire shape: Go field names (requires.CandidateProvenance).
			wire := make([]map[string]any, len(want))
			for i, c := range want {
				wire[i] = map[string]any{"GeneratorName": c.Generator, "Score": c.Score, "Weight": c.Weight, "Metadata": c.Metadata}
			}
			raw, err := json.Marshal(wire)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := results.DecodeContributions(raw)
			if err != nil {
				t.Fatalf("decode %s: %v", raw, err)
			}
			if len(got) != len(want) {
				t.Fatalf("got %d contributions, want %d", len(got), len(want))
			}
			for i := range want {
				if !reflect.DeepEqual(got[i], want[i]) {
					t.Fatalf("contribution %d: got %+v want %+v", i, got[i], want[i])
				}
			}
		})
	})
})
