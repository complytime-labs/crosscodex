package crosswalk_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/test/eval/crosswalk"
)

var _ = Describe("Precision Property Specifications", Ordered, func() {
	// Targets of the synthetic catalog that no profile reference names, so
	// the drawn rows and labels vary over a fixed, valid universe.
	targets := []string{"ab-2", "ab-2.3", "ab-2.3.1", "cd-1", "cd-9", "top.1"}
	sources := []string{"s-1", "s-2"}
	verdicts := []string{"related", "unrelated", "disputed"}

	It("keeps every precision rate in [0, 1] and coverage within the produced rows", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			var ms []crosswalk.Mapping
			labels := map[crosswalk.Pair]crosswalk.Label{}
			for _, s := range sources {
				for _, tg := range targets {
					if rapid.Bool().Draw(t, "produced "+s+tg) {
						ms = append(ms, mapping(s, tg, equivalent, 0.8))
					}
					if rapid.Bool().Draw(t, "labelled "+s+tg) {
						labels[crosswalk.Pair{SourceID: s, TargetID: tg}] = crosswalk.Label{Verdict: rapid.SampledFrom(verdicts).Draw(t, "verdict "+s+tg)}
					}
				}
			}
			in := scoreInput([]crosswalk.Reference{ref("s-1", "ab-2"), ref("s-2", "cd-1")}, ms, candidatesFor(ms))
			in.Labels = labels
			r, err := crosswalk.Score(in)
			if err != nil {
				t.Fatalf("Score: %v", err)
			}
			p := r.Precision
			rates := []crosswalk.Rate{p.Reference}
			if len(labels) > 0 {
				rates = append(rates, *p.Labelled, *p.Coverage)
				if p.Coverage.Hit > len(ms) || p.Coverage.Total != len(ms) {
					t.Fatalf("coverage %v with %d produced rows", p.Coverage, len(ms))
				}
				if p.Labelled.Total != p.Coverage.Hit {
					t.Fatalf("labelled total %d != coverage hit %d", p.Labelled.Total, p.Coverage.Hit)
				}
				if got := p.Coverage.Hit + len(p.Unlabelled) + len(p.Disputed); got != len(ms) {
					t.Fatalf("judged %d + unlabelled %d + disputed %d != %d rows", p.Coverage.Hit, len(p.Unlabelled), len(p.Disputed), len(ms))
				}
			}
			for _, rt := range rates {
				if rt.Rate < 0 || rt.Rate > 1 || rt.Hit > rt.Total {
					t.Fatalf("rate out of range: %+v", rt)
				}
			}
		})
	})

	It("lets a human entry win whatever the entry order", func() {
		dir := GinkgoT().TempDir()
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			human := rapid.SampledFrom(verdicts).Draw(t, "human verdict")
			model := rapid.SampledFrom(verdicts).Draw(t, "model verdict")
			h := entry(map[string]string{"verdict": human, "judged_by": "[human]"})
			m := entry(map[string]string{"verdict": model, "judged_by": "[opus]"})
			body := "version: 1\npairs:\n" + h + m
			if rapid.Bool().Draw(t, "model first") {
				body = "version: 1\npairs:\n" + m + h
			}
			path := filepath.Join(dir, "labels.yaml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("writing labels: %v", err)
			}
			got, _, err := crosswalk.LoadLabels(path)
			if err != nil {
				t.Fatalf("LoadLabels: %v", err)
			}
			if v := got[crosswalk.Pair{SourceID: "PO.1.1", TargetID: "sa-8"}].Verdict; v != human {
				t.Fatalf("verdict %q, want the human's %q", v, human)
			}
		})
	})
})
