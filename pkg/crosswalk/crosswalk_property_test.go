package crosswalk_test

import (
	"reflect"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/crosswalk"
)

var invertible = []pb.RelationshipType{equivalent, superset, subset, complements, partial, conflicts, none}
var related = []pb.RelationshipType{equivalent, superset, subset, contributes, complements, partial, conflicts}

var pathsGen = func() *rapid.Generator[[]crosswalk.Path] {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return rapid.SliceOfN(rapid.Custom(func(t *rapid.T) crosswalk.Path {
		job := rapid.SampledFrom([]string{"job-a", "job-b", "job-c"}).Draw(t, "job")
		return crosswalk.Path{
			Hub:        rapid.SampledFrom([]string{"h1", "h2", "h3"}).Draw(t, "hub"),
			Type:       rapid.SampledFrom([]pb.RelationshipType{unspecified, subset, superset, equivalent}).Draw(t, "type"),
			Confidence: rapid.SampledFrom([]float64{0.25, 0.5, 0.75}).Draw(t, "confidence"),
			Jobs:       []string{job},
			LatestJob:  job,
			LatestAt:   base.Add(time.Duration(rapid.IntRange(0, 2).Draw(t, "hours")) * time.Hour),
		}
	}), 1, 8)
}()

var _ = Describe("Property Specifications", Ordered, func() {
	It("LineageSegment is empty iff the title is blank, else starts with the trimmed title", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			title := rapid.String().Draw(t, "title")
			label := rapid.String().Draw(t, "label")
			identifier := rapid.String().Draw(t, "identifier")
			got := crosswalk.LineageSegment(title, label, identifier)
			trimmed := strings.TrimSpace(title)
			if (got == "") != (trimmed == "") {
				t.Fatalf("LineageSegment(%q, %q, %q) = %q: empty must match a blank title", title, label, identifier, got)
			}
			if !strings.HasPrefix(got, trimmed) {
				t.Fatalf("LineageSegment(%q, %q, %q) = %q: must start with %q", title, label, identifier, got, trimmed)
			}
		})
	})

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

	It("Collapse keeps its counts, hubs and verdict consistent with the paths", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			ps := pathsGen.Draw(t, "paths")
			c := crosswalk.Collapse(ps)
			if c.PathsTotal != len(ps) || c.PathsSupporting > c.PathsTotal || c.PathsSupporting < 1 {
				t.Fatalf("supporting %d, total %d, paths %d", c.PathsSupporting, c.PathsTotal, len(ps))
			}
			if !slices.ContainsFunc(ps, func(p crosswalk.Path) bool { return p.Confidence == c.Confidence }) {
				t.Fatalf("confidence %v is not any path's confidence", c.Confidence)
			}
			if !slices.IsSorted(c.Hubs) || len(slices.Compact(slices.Clone(c.Hubs))) != len(c.Hubs) {
				t.Fatalf("hubs not sorted and distinct: %v", c.Hubs)
			}
			if c.Type != unspecified {
				for _, p := range ps {
					if p.Type != unspecified && p.Type != c.Type {
						t.Fatalf("verdict %v but a named path has %v", c.Type, p.Type)
					}
				}
			}
			if c.JobsAgreeing > len(c.Jobs) {
				t.Fatalf("agreeing %d exceeds jobs %v", c.JobsAgreeing, c.Jobs)
			}
		})
	})

	It("Collapse does not depend on the order of its paths", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			ps := pathsGen.Draw(t, "paths")
			perm := rapid.Permutation(ps).Draw(t, "perm")
			if got, want := crosswalk.Collapse(perm), crosswalk.Collapse(ps); !reflect.DeepEqual(got, want) {
				t.Fatalf("Collapse changed with path order:\n got  %+v\n want %+v", got, want)
			}
		})
	})

	Describe("EquivalenceNotes", func() {
		nodes := []string{"n1", "n2", "n3"}
		catalogs := []string{"c1", "c2"}
		kinds := []pb.RelationshipType{equivalent, partial}
		dirs := []crosswalk.Direction{crosswalk.Direct, crosswalk.Derived}
		rowGen := rapid.Custom(func(t *rapid.T) *pb.CrosswalkRow {
			src := rapid.SampledFrom(nodes).Draw(t, "src")
			tgt := rapid.SampledFrom(nodes).Draw(t, "tgt")
			return &pb.CrosswalkRow{
				Source:           &pb.CrosswalkSide{NodeId: src, CatalogId: rapid.SampledFrom(catalogs).Draw(t, "srcCat"), Identifier: "id-" + src},
				Target:           &pb.CrosswalkSide{NodeId: tgt, CatalogId: rapid.SampledFrom(catalogs).Draw(t, "tgtCat"), Identifier: "id-" + tgt},
				RelationshipType: rapid.SampledFrom(kinds).Draw(t, "type"),
				Direction:        string(rapid.SampledFrom(dirs).Draw(t, "dir")),
			}
		})

		It("is aligned with rows and leaves non-equivalent or non-direct rows empty", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				rows := rapid.SliceOfN(rowGen, 0, 10).Draw(t, "rows")
				notes := crosswalk.EquivalenceNotes(rows)
				if len(notes) != len(rows) {
					t.Fatalf("len(notes) %d != len(rows) %d", len(notes), len(rows))
				}
				for i, r := range rows {
					if (r.RelationshipType != equivalent || r.Direction != string(crosswalk.Direct)) && notes[i] != "" {
						t.Fatalf("row %d (%v, %q) got note %q", i, r.RelationshipType, r.Direction, notes[i])
					}
				}
			})
		})

		It("does not depend on input order", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				rows := rapid.SliceOfN(rowGen, 0, 10).Draw(t, "rows")
				perm := rapid.Permutation(rows).Draw(t, "perm")
				byRow := map[*pb.CrosswalkRow]string{}
				for i, n := range crosswalk.EquivalenceNotes(rows) {
					byRow[rows[i]] = n
				}
				for i, n := range crosswalk.EquivalenceNotes(perm) {
					if byRow[perm[i]] != n {
						t.Fatalf("row %v: note %q after shuffle, %q before", perm[i], n, byRow[perm[i]])
					}
				}
			})
		})
	})
})
