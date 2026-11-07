package candidate_test

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
)

// Suite bootstrap lives in candidate_bdd_test.go.

// fusionInput is one drawn rank-fusion scenario: each generator's candidates
// and the per-source cap.
type fusionInput struct {
	sets         map[string][]candidate.Candidate
	maxPerSource int
}

// drawFusionInput draws 1-3 generators, each listing a distinct subset of six
// targets for up to three sources. Scores come from a small set so ties occur.
func drawFusionInput(t *rapid.T) fusionInput {
	sources := []string{"S0", "S1", "S2"}
	targets := []string{"T0", "T1", "T2", "T3", "T4", "T5"}
	in := fusionInput{
		sets:         map[string][]candidate.Candidate{},
		maxPerSource: rapid.IntRange(1, 7).Draw(t, "maxPerSource"),
	}
	for g := range rapid.IntRange(1, 3).Draw(t, "generators") {
		name := fmt.Sprintf("g%d", g)
		weight := rapid.SampledFrom([]float64{0.5, 1}).Draw(t, name+".weight")
		var set []candidate.Candidate
		for _, source := range sources {
			picked := rapid.SliceOfDistinct(rapid.SampledFrom(targets), func(s string) string { return s }).Draw(t, name+"."+source)
			for _, target := range picked {
				set = append(set, candidate.Candidate{
					SourceID: source, TargetID: target, Weight: weight, GeneratorID: name,
					Score: rapid.SampledFrom([]float64{0.1, 0.5, 0.9, 1}).Draw(t, "score"),
				})
			}
		}
		in.sets[name] = set
	}
	return in
}

func fuseSets(t *rapid.T, sets map[string][]candidate.Candidate, maxPerSource int) []candidate.Candidate {
	registry := candidate.NewRegistry()
	for name, set := range sets {
		if err := registry.Register(&mockGenerator{name: name, candidates: set}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	out, err := registry.Generate(context.Background(), candidate.GenerateRequest{}, candidate.StrategyRankFusion, candidate.WithMaxPerSource(maxPerSource))
	if err != nil {
		t.Fatalf("rank fusion: %v", err)
	}
	return out
}

var _ = Describe("Property Specifications", Ordered, func() {
	Describe("rank fusion", func() {
		It("keeps min(maxPerSource, distinct targets) candidates per source", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				in := drawFusionInput(t)
				distinct := map[string]map[string]bool{}
				for _, set := range in.sets {
					for _, c := range set {
						if distinct[c.SourceID] == nil {
							distinct[c.SourceID] = map[string]bool{}
						}
						distinct[c.SourceID][c.TargetID] = true
					}
				}
				got := map[string]int{}
				for _, c := range fuseSets(t, in.sets, in.maxPerSource) {
					got[c.SourceID]++
				}
				for source, targets := range distinct {
					if want := min(in.maxPerSource, len(targets)); got[source] != want {
						t.Fatalf("source %s: got %d candidates, want %d", source, got[source], want)
					}
				}
				if len(got) != len(distinct) {
					t.Fatalf("got sources %v, want %v", got, distinct)
				}
			})
		})

		It("orders by source ascending, score descending, target ascending, without repeating a pair", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				in := drawFusionInput(t)
				out := fuseSets(t, in.sets, in.maxPerSource)
				seen := map[[2]string]bool{}
				for i, c := range out {
					pair := [2]string{c.SourceID, c.TargetID}
					if seen[pair] {
						t.Fatalf("pair %v appears twice", pair)
					}
					seen[pair] = true
					if i == 0 {
						continue
					}
					p := out[i-1]
					ordered := p.SourceID < c.SourceID ||
						(p.SourceID == c.SourceID && (p.Score > c.Score || (p.Score == c.Score && p.TargetID < c.TargetID)))
					if !ordered {
						t.Fatalf("candidate %d %+v is out of order after %+v", i, c, p)
					}
				}
			})
		})

		It("scores each pair as the sum of weight/(60+rank) over generators sorted by name, with ranks within each generator's list", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				in := drawFusionInput(t)
				listed := map[[2]string]int{}
				for _, set := range in.sets {
					for _, c := range set {
						listed[[2]string{c.GeneratorID, c.SourceID}]++
					}
				}
				for _, c := range fuseSets(t, in.sets, in.maxPerSource) {
					names := make([]string, len(c.Contributions))
					want := 0.0
					for i, contribution := range c.Contributions {
						names[i] = contribution.GeneratorID
						if n := float64(listed[[2]string{contribution.GeneratorID, c.SourceID}]); contribution.Rank < 1 || contribution.Rank > n {
							t.Fatalf("%s->%s: %s rank %v outside [1, %v]", c.SourceID, c.TargetID, contribution.GeneratorID, contribution.Rank, n)
						}
						want += contribution.Weight / (60 + contribution.Rank)
					}
					if !sort.StringsAreSorted(names) {
						t.Fatalf("%s->%s: contributions %v not sorted by generator", c.SourceID, c.TargetID, names)
					}
					if c.GeneratorID != strings.Join(names, "+") {
						t.Fatalf("%s->%s: GeneratorID %q, want %q", c.SourceID, c.TargetID, c.GeneratorID, strings.Join(names, "+"))
					}
					if c.Score != want {
						t.Fatalf("%s->%s: score %v, want %v", c.SourceID, c.TargetID, c.Score, want)
					}
				}
			})
		})

		It("returns the same result whatever order each generator lists its candidates in", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				in := drawFusionInput(t)
				shuffled := map[string][]candidate.Candidate{}
				for name, set := range in.sets {
					shuffled[name] = rapid.Permutation(set).Draw(t, name+".order")
				}
				want := fuseSets(t, in.sets, in.maxPerSource)
				if got := fuseSets(t, shuffled, in.maxPerSource); !reflect.DeepEqual(got, want) {
					t.Fatalf("shuffled input changed the result:\n got %+v\nwant %+v", got, want)
				}
			})
		})
	})
})
