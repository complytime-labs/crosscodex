package crosswalk_test

import (
	"reflect"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/crosswalk"
)

var invertible = []pb.RelationshipType{equivalent, superset, subset, complements, partial, conflicts, none}
var related = []pb.RelationshipType{equivalent, superset, subset, contributes, complements, partial, conflicts}

var _ = Describe("Property Specifications", Ordered, func() {
	It("Invert is an involution on invertible types", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			typ := rapid.SampledFrom(invertible).Draw(t, "type")
			once, ok := crosswalk.Invert(typ)
			twice, ok2 := crosswalk.Invert(once)
			if !ok || !ok2 || twice != typ {
				t.Fatalf("Invert(Invert(%v)) = %v (ok %v, %v)", typ, twice, ok, ok2)
			}
		})
	})

	It("EQUIVALENT is the identity of Compose for related types", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			typ := rapid.SampledFrom(related).Draw(t, "type")
			left, lok := crosswalk.Compose(equivalent, typ)
			right, rok := crosswalk.Compose(typ, equivalent)
			if !lok || !rok || left != typ || right != typ {
				t.Fatalf("Compose identity broke for %v: left %v/%v right %v/%v", typ, left, lok, right, rok)
			}
		})
	})

	It("Merge ignores input order and never agrees more jobs than it judged", func() {
		base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		verdicts := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) crosswalk.Verdict {
			return crosswalk.Verdict{
				JobID:       rapid.SampledFrom([]string{"job-a", "job-b", "job-c"}).Draw(t, "job"),
				CompletedAt: base.Add(time.Duration(rapid.IntRange(0, 2).Draw(t, "hours")) * time.Hour),
				Type:        rapid.SampledFrom(append([]pb.RelationshipType{unspecified}, related...)).Draw(t, "type"),
				Confidence:  rapid.SampledFrom([]float64{0.25, 0.5, 0.75}).Draw(t, "confidence"),
				Reversed:    rapid.Bool().Draw(t, "reversed"),
			}
		}), 1, 8)
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			vs := verdicts.Draw(t, "verdicts")
			shuffled := rapid.Permutation(vs).Draw(t, "shuffled")
			a, b := crosswalk.Merge(vs), crosswalk.Merge(shuffled)
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("Merge depends on input order: %+v vs %+v", a, b)
			}
			if a.JobsAgreeing > a.JobsJudged || a.JobsAgreeing < 1 {
				t.Fatalf("agreeing %d outside [1, judged %d]", a.JobsAgreeing, a.JobsJudged)
			}
		})
	})
})
