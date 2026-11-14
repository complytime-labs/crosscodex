package crosswalk_test

import (
	"encoding/json"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/test/eval/crosswalk"
)

var _ = Describe("Property Specifications", Ordered, func() {
	It("Canonical is idempotent", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			s := rapid.String().Draw(t, "s")
			once := crosswalk.Canonical(s)
			if twice := crosswalk.Canonical(once); twice != once {
				t.Fatalf("Canonical(%q) = %q but Canonical of that = %q", s, once, twice)
			}
		})
	})

	It("Canonical output has no leading or trailing dot", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			s := rapid.String().Draw(t, "s")
			out := crosswalk.Canonical(s)
			if out != "" && (out[0] == '.' || out[len(out)-1] == '.') {
				t.Fatalf("Canonical(%q) = %q has a leading or trailing dot", s, out)
			}
		})
	})

	It("resolving a control's own ID yields that control", func() {
		c, err := crosswalk.LoadCatalog(syntheticCatalog)
		Expect(err).NotTo(HaveOccurred())
		x := crosswalk.NewTargetIndex(c)
		ids := []string{"ab-2", "ab-2.3", "ab-2.3.1", "top.1"}
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			id := rapid.SampledFrom(ids).Draw(t, "id")
			got := x.Resolve(id)
			if got.State != crosswalk.Resolved || got.ControlID != id {
				t.Fatalf("Resolve(%q) = %+v", id, got)
			}
		})
	})

	Context("scoring", func() {
		var src, target *crosswalk.Catalog
		pairs := [][2]string{}
		BeforeAll(func() {
			src = mustCatalog("testdata/source_a.json")
			target = mustCatalog(syntheticCatalog)
			for _, s := range []string{"s-1", "s-2"} {
				for _, t := range []string{"ab-2", "ab-2.3", "ab-2.3.1", "cd-1", "cd-9", "top.1"} {
					pairs = append(pairs, [2]string{s, t})
				}
			}
		})

		// drawInput draws distinct references, candidates and mappings, every
		// mapping a candidate, so the input satisfies the pipeline invariant.
		drawInput := func(t *rapid.T) crosswalk.ScoreInput {
			var refs []crosswalk.Reference
			var cs []crosswalk.Candidate
			var ms []crosswalk.Mapping
			for i, p := range pairs {
				if rapid.Bool().Draw(t, fmt.Sprintf("ref%d", i)) {
					refs = append(refs, crosswalk.Reference{SourceID: p[0], TargetID: p[1]})
				}
				if !rapid.Bool().Draw(t, fmt.Sprintf("cand%d", i)) {
					continue
				}
				gens := rapid.SliceOfDistinct(rapid.SampledFrom([]string{"embedding", "lexical"}), rapid.ID[string]).Draw(t, fmt.Sprintf("gens%d", i))
				cs = append(cs, crosswalk.Candidate{SourceID: p[0], TargetID: p[1], Generators: gens})
				if rapid.Bool().Draw(t, fmt.Sprintf("map%d", i)) {
					ms = append(ms, crosswalk.Mapping{
						SourceID: p[0], TargetID: p[1],
						Type:       rapid.SampledFrom([]string{"RELATIONSHIP_TYPE_EQUIVALENT", "RELATIONSHIP_TYPE_PARTIAL"}).Draw(t, fmt.Sprintf("type%d", i)),
						Confidence: rapid.Float64Range(0, 1).Draw(t, fmt.Sprintf("conf%d", i)),
					})
				}
			}
			if len(refs) == 0 {
				refs = []crosswalk.Reference{{SourceID: "s-1", TargetID: "ab-2"}}
			}
			return crosswalk.ScoreInput{
				Profile: &crosswalk.Profile{Name: "p", Source: "s", Target: "t", Controls: []string{"s-1", "s-2"}, Digest: "d"},
				Source:  src, Subset: src, Target: target,
				Refs: crosswalk.References{Resolved: refs},
				Run:  &crosswalk.Run{JobID: "j", Analysis: json.RawMessage(scoreAnalysis), Mappings: ms, Candidates: cs},
			}
		}

		It("found exact implies found base implies reachable base, and found exact implies reachable exact", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				r, err := crosswalk.Score(drawInput(t))
				if err != nil {
					t.Fatalf("Score: %v", err)
				}
				for _, ref := range r.References {
					if ref.FoundExact && !ref.FoundBase {
						t.Fatalf("%+v: found exact but not found base", ref)
					}
					if ref.ReachableExact && !ref.ReachableBase {
						t.Fatalf("%+v: reachable exact but not reachable base", ref)
					}
					if ref.FoundExact && !ref.ReachableExact || ref.FoundBase && !ref.ReachableBase {
						t.Fatalf("%+v: found but not reachable", ref)
					}
				}
			})
		})

		It("Jaccard is in [0, 1], and 1 against itself whenever there are related pairs", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				in := drawInput(t)
				self, err := crosswalk.Score(in)
				if err != nil {
					t.Fatalf("Score: %v", err)
				}
				in.Baseline = self
				r, err := crosswalk.Score(in)
				if err != nil {
					t.Fatalf("Score against itself: %v", err)
				}
				if j := r.Agreement.Jaccard.Rate; len(in.Run.Mappings) > 0 && j != 1 {
					t.Fatalf("Jaccard against itself = %v with %d related pairs", j, len(in.Run.Mappings))
				}
				other := drawInput(t)
				other.Refs, other.Baseline = in.Refs, self
				r, err = crosswalk.Score(other)
				if err != nil {
					t.Fatalf("Score against another run: %v", err)
				}
				if j := r.Agreement.Jaccard.Rate; j < 0 || j > 1 {
					t.Fatalf("Jaccard = %v, outside [0, 1]", j)
				}
			})
		})
	})
})
