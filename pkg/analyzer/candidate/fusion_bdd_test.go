package candidate_test

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
)

func scored(gen, source string, weight float64, targets map[string]float64) []candidate.Candidate {
	out := make([]candidate.Candidate, 0, len(targets))
	for target, score := range targets {
		out = append(out, candidate.Candidate{
			SourceID: source, TargetID: target, Score: score, Weight: weight, GeneratorID: gen,
		})
	}
	return out
}

func fuse(gens []*mockGenerator, opts ...candidate.GenerateOption) ([]candidate.Candidate, error) {
	registry := candidate.NewRegistry()
	for _, g := range gens {
		Expect(registry.Register(g)).To(Succeed())
	}
	return registry.Generate(context.Background(), candidate.GenerateRequest{}, candidate.StrategyRankFusion, opts...)
}

func targetIDs(cs []candidate.Candidate) []string {
	ids := make([]string, 0, len(cs))
	for _, c := range cs {
		ids = append(ids, c.TargetID)
	}
	return ids
}

var _ = Describe("Rank fusion", func() {
	It("ranks a pair found by two generators above a pair found by one", func() {
		a := &mockGenerator{name: "a", candidates: scored("a", "S", 1, map[string]float64{"T1": 0.9, "T2": 0.8})}
		b := &mockGenerator{name: "b", candidates: scored("b", "S", 1, map[string]float64{"T2": 0.7})}

		results, err := fuse([]*mockGenerator{a, b}, candidate.WithMaxPerSource(10))
		Expect(err).NotTo(HaveOccurred())
		Expect(targetIDs(results)).To(Equal([]string{"T2", "T1"}))
		Expect(results[0].Score).To(BeNumerically("~", 1.0/62+1.0/61, 1e-12))
		Expect(results[0].GeneratorID).To(Equal("a+b"))
		Expect(results[0].Weight).To(Equal(1.0))
		Expect(results[0].Metadata).To(BeNil())
		Expect(results[1].GeneratorID).To(Equal("a"))
	})

	It("assigns tied scores the mean of their positions", func() {
		g := &mockGenerator{name: "a", candidates: scored("a", "S", 1,
			map[string]float64{"T1": 0.9, "T2": 0.5, "T3": 0.5, "T4": 0.5})}

		results, err := fuse([]*mockGenerator{g}, candidate.WithMaxPerSource(10))
		Expect(err).NotTo(HaveOccurred())
		ranks := map[string]float64{}
		for _, r := range results {
			ranks[r.TargetID] = r.Contributions[0].Rank
		}
		Expect(ranks).To(Equal(map[string]float64{"T1": 1, "T2": 3, "T3": 3, "T4": 3}))
	})

	It("lets a flat-scored generator fill ranks without displacing a graded one", func() {
		graded := map[string]float64{}
		for i := 1; i <= 10; i++ {
			graded[fmt.Sprintf("T%02d", i)] = 1 - float64(i)/100
		}
		flat := map[string]float64{}
		for i := 0; i < 100; i++ {
			flat[fmt.Sprintf("X%03d", i)] = 1
		}
		sem := &mockGenerator{name: "semantic", candidates: scored("semantic", "S", 1, graded)}
		bin := &mockGenerator{name: "binary", candidates: scored("binary", "S", 1, flat)}

		results, err := fuse([]*mockGenerator{sem, bin}, candidate.WithMaxPerSource(10))
		Expect(err).NotTo(HaveOccurred())
		Expect(targetIDs(results)).To(Equal([]string{
			"T01", "T02", "T03", "T04", "T05", "T06", "T07", "T08", "T09", "T10",
		}))
	})

	It("caps candidates per source", func() {
		var cands []candidate.Candidate
		for _, s := range []string{"S1", "S2"} {
			cands = append(cands, scored("a", s, 1, map[string]float64{
				"T1": 0.9, "T2": 0.8, "T3": 0.7, "T4": 0.6, "T5": 0.5,
			})...)
		}
		results, err := fuse([]*mockGenerator{{name: "a", candidates: cands}}, candidate.WithMaxPerSource(3))
		Expect(err).NotTo(HaveOccurred())
		Expect(results).To(HaveLen(6))
		Expect(targetIDs(results)).To(Equal([]string{"T1", "T2", "T3", "T1", "T2", "T3"}))
		Expect(results[0].SourceID).To(Equal("S1"))
		Expect(results[3].SourceID).To(Equal("S2"))
	})

	It("scales the fused score by generator weight", func() {
		a := &mockGenerator{name: "a", candidates: scored("a", "S", 1.0, map[string]float64{"T1": 0.9})}
		b := &mockGenerator{name: "b", candidates: scored("b", "S", 0.5, map[string]float64{"T2": 0.9})}

		results, err := fuse([]*mockGenerator{a, b}, candidate.WithMaxPerSource(10))
		Expect(err).NotTo(HaveOccurred())
		Expect(targetIDs(results)).To(Equal([]string{"T1", "T2"}))
		Expect(results[0].Score).To(BeNumerically("~", 1.0/61, 1e-12))
		Expect(results[1].Score).To(BeNumerically("~", 0.5/61, 1e-12))
	})

	It("breaks fused-score ties on target ID ascending", func() {
		g := &mockGenerator{name: "a", candidates: scored("a", "S", 1, map[string]float64{"T3": 0.5, "T1": 0.5, "T2": 0.5})}
		results, err := fuse([]*mockGenerator{g}, candidate.WithMaxPerSource(2))
		Expect(err).NotTo(HaveOccurred())
		Expect(targetIDs(results)).To(Equal([]string{"T1", "T2"}))
	})

	It("is deterministic across runs", func() {
		var gens []*mockGenerator
		for _, name := range []string{"a", "b", "c", "d"} {
			targets := map[string]float64{}
			for i := 0; i < 30; i++ {
				targets[fmt.Sprintf("T%02d", i)] = float64((i*7+len(name)*3)%11) / 10
			}
			gens = append(gens, &mockGenerator{name: name, candidates: scored(name, "S", 0.7, targets)})
		}
		first, err := fuse(gens, candidate.WithMaxPerSource(15))
		Expect(err).NotTo(HaveOccurred())
		for i := 0; i < 20; i++ {
			again, err := fuse(gens, candidate.WithMaxPerSource(15))
			Expect(err).NotTo(HaveOccurred())
			Expect(again).To(Equal(first))
		}
	})

	It("records contributions sorted by generator ID with score, weight and metadata", func() {
		a := &mockGenerator{name: "a", candidates: []candidate.Candidate{
			{SourceID: "S", TargetID: "T", Score: 0.4, Weight: 0.3, GeneratorID: "a", Metadata: map[string]string{"k": "va"}},
		}}
		b := &mockGenerator{name: "b", candidates: []candidate.Candidate{
			{SourceID: "S", TargetID: "T", Score: 0.9, Weight: 0.8, GeneratorID: "b", Metadata: map[string]string{"k": "vb"}},
		}}

		results, err := fuse([]*mockGenerator{b, a}, candidate.WithMaxPerSource(10))
		Expect(err).NotTo(HaveOccurred())
		Expect(results).To(HaveLen(1))
		Expect(results[0].Contributions).To(Equal([]candidate.Contribution{
			{GeneratorID: "a", Score: 0.4, Weight: 0.3, Rank: 1, Metadata: map[string]string{"k": "va"}},
			{GeneratorID: "b", Score: 0.9, Weight: 0.8, Rank: 1, Metadata: map[string]string{"k": "vb"}},
		}))
	})

	It("ranks within each generator and source independently", func() {
		cands := append(
			scored("a", "S1", 1, map[string]float64{"T1": 0.9}),
			scored("a", "S2", 1, map[string]float64{"T1": 0.1})...,
		)
		results, err := fuse([]*mockGenerator{{name: "a", candidates: cands}}, candidate.WithMaxPerSource(10))
		Expect(err).NotTo(HaveOccurred())
		Expect(results).To(HaveLen(2))
		Expect(results[0].Contributions[0].Rank).To(Equal(1.0))
		Expect(results[1].Contributions[0].Rank).To(Equal(1.0))
	})

	DescribeTable("rejects a missing or non-positive max per source",
		func(opts ...candidate.GenerateOption) {
			g := &mockGenerator{name: "a", candidates: scored("a", "S", 1, map[string]float64{"T1": 0.9})}
			_, err := fuse([]*mockGenerator{g}, opts...)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("max per source"))
		},
		Entry("no option"),
		Entry("zero", candidate.WithMaxPerSource(0)),
		Entry("negative", candidate.WithMaxPerSource(-1)),
	)

	It("selects the same pairs as union for one generator within the cap", func() {
		g := &mockGenerator{name: "a", candidates: scored("a", "S", 1, map[string]float64{"T1": 0.9, "T2": 0.8, "T3": 0.7})}
		fused, err := fuse([]*mockGenerator{g}, candidate.WithMaxPerSource(10))
		Expect(err).NotTo(HaveOccurred())

		registry := candidate.NewRegistry()
		Expect(registry.Register(g)).To(Succeed())
		union, err := registry.Generate(context.Background(), candidate.GenerateRequest{}, candidate.StrategyUnion)
		Expect(err).NotTo(HaveOccurred())

		pairs := func(cs []candidate.Candidate) []string {
			out := make([]string, 0, len(cs))
			for _, c := range cs {
				out = append(out, c.SourceID+":"+c.TargetID)
			}
			return out
		}
		Expect(pairs(fused)).To(ConsistOf(pairs(union)))
	})

	It("leaves Contributions nil for union and weighted_union", func() {
		g := &mockGenerator{name: "a", candidates: scored("a", "S", 1, map[string]float64{"T1": 0.9})}
		registry := candidate.NewRegistry()
		Expect(registry.Register(g)).To(Succeed())
		for _, strategy := range []candidate.AggregationStrategy{candidate.StrategyUnion, candidate.StrategyWeightedUnion} {
			results, err := registry.Generate(context.Background(), candidate.GenerateRequest{}, strategy)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].Contributions).To(BeNil())
		}
	})
})
