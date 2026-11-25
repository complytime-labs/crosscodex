package crosswalk_test

import (
	"fmt"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/test/eval/crosswalk"
)

var _ = Describe("Labels Merge Property Specifications", Ordered, func() {
	verdicts := []string{"related", "unrelated"}
	ids := rapid.SampledFrom([]string{"a", "b", "c", "d", "PO.1.1", "sa-8"})

	// drawMerge draws a required pair set, two covering judges and existing
	// labels over a slightly larger universe (so some are outside the set,
	// some human, some model).
	drawMerge := func(t *rapid.T) ([]crosswalk.Label, map[string][]crosswalk.JudgeEntry, []crosswalk.Pair) {
		var pairs []crosswalk.Pair
		judges := map[string][]crosswalk.JudgeEntry{"opus": nil, "sonnet": nil}
		var existing []crosswalk.Label
		seen := map[crosswalk.Pair]bool{}
		n := rapid.IntRange(1, 6).Draw(t, "pairs")
		for i := 0; i < n; i++ {
			p := crosswalk.Pair{SourceID: ids.Draw(t, "source"), TargetID: ids.Draw(t, "target")}
			if seen[p] {
				continue
			}
			seen[p] = true
			pairs = append(pairs, p)
			for _, j := range []string{"opus", "sonnet"} {
				judges[j] = append(judges[j], crosswalk.JudgeEntry{
					Source: p.SourceID, Target: p.TargetID,
					Verdict:   rapid.SampledFrom(verdicts).Draw(t, "verdict "+j),
					Rationale: fmt.Sprintf("%s on %s", j, p),
				})
			}
			if rapid.Bool().Draw(t, "existing") {
				by := rapid.SampledFrom([]string{"human", "opus"}).Draw(t, "by")
				existing = append(existing, crosswalk.Label{
					Source: p.SourceID, Target: p.TargetID, Verdict: "related",
					JudgedBy: []string{by}, Rationale: "existing", Date: "2026-01-01",
				})
			}
		}
		if rapid.Bool().Draw(t, "outside") {
			outside := crosswalk.Label{Source: "outside", Target: "pair", Verdict: "unrelated", JudgedBy: []string{"sonnet"}, Rationale: "o", Date: "2026-01-01"}
			existing = append(existing, outside)
		}
		return existing, judges, pairs
	}

	It("is idempotent: merging its own output again yields identical bytes", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			existing, judges, pairs := drawMerge(t)
			first, _, err := crosswalk.MergeLabels(existing, judges, pairs, mergeDate)
			if err != nil {
				t.Fatalf("MergeLabels: %v", err)
			}
			second, _, err := crosswalk.MergeLabels(first, judges, pairs, mergeDate)
			if err != nil {
				t.Fatalf("second MergeLabels: %v", err)
			}
			a, err := crosswalk.RenderLabels(first)
			if err != nil {
				t.Fatalf("RenderLabels: %v", err)
			}
			b, err := crosswalk.RenderLabels(second)
			if err != nil {
				t.Fatalf("RenderLabels: %v", err)
			}
			if string(a) != string(b) {
				t.Fatalf("not idempotent:\n%s\n---\n%s", a, b)
			}
		})
	})

	It("never drops or alters a human entry, and always renders output the strict loader accepts", func() {
		path := filepath.Join(GinkgoT().TempDir(), "labels.yaml")
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			existing, judges, pairs := drawMerge(t)
			merged, _, err := crosswalk.MergeLabels(existing, judges, pairs, mergeDate)
			if err != nil {
				t.Fatalf("MergeLabels: %v", err)
			}
			for _, e := range existing {
				if e.JudgedBy[0] != "human" {
					continue
				}
				found := false
				for _, m := range merged {
					if m.Source == e.Source && m.Target == e.Target {
						found = true
						if fmt.Sprint(m) != fmt.Sprint(e) {
							t.Fatalf("human entry altered: %+v -> %+v", e, m)
						}
					}
				}
				if !found {
					t.Fatalf("human entry dropped: %+v", e)
				}
			}
			raw, err := crosswalk.RenderLabels(merged)
			if err != nil {
				t.Fatalf("RenderLabels: %v", err)
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatalf("writing rendered labels: %v", err)
			}
			if _, _, err := crosswalk.LoadLabels(path); err != nil {
				t.Fatalf("loader rejected output: %v\n%s", err, raw)
			}
		})
	})

	It("covers every required pair exactly once", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			existing, judges, pairs := drawMerge(t)
			merged, _, err := crosswalk.MergeLabels(existing, judges, pairs, mergeDate)
			if err != nil {
				t.Fatalf("MergeLabels: %v", err)
			}
			count := map[crosswalk.Pair]int{}
			for _, m := range merged {
				count[crosswalk.Pair{SourceID: m.Source, TargetID: m.Target}]++
			}
			for _, p := range pairs {
				if count[p] != 1 {
					t.Fatalf("pair %s appears %d times", p, count[p])
				}
			}
		})
	})
})
