package crosswalk_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/test/eval/crosswalk"
)

const (
	equivalent = "RELATIONSHIP_TYPE_EQUIVALENT"
	subsetOf   = "RELATIONSHIP_TYPE_SUBSET_OF"
	// scoreAnalysis is the "analysis" of a results export, as protojson writes it.
	scoreAnalysis = `{"sourceCatalog":{"catalogId":"cat-src","name":"Source A"},"targetCatalog":{"catalogId":"cat-tgt","name":"Synthetic"},"bidirectional":false}`
)

func ref(source, target string) crosswalk.Reference {
	return crosswalk.Reference{SourceID: source, TargetID: target, Citation: "cite " + target}
}

func mapping(source, target, typ string, confidence float64) crosswalk.Mapping {
	return crosswalk.Mapping{SourceID: source, TargetID: target, Type: typ, Confidence: confidence}
}

func cand(source, target string, generators ...string) crosswalk.Candidate {
	return crosswalk.Candidate{SourceID: source, TargetID: target, Generators: generators}
}

// candidatesFor returns one "embedding" candidate per mapping, so the input
// satisfies the found ⊆ reachable invariant.
func candidatesFor(ms []crosswalk.Mapping) []crosswalk.Candidate {
	out := make([]crosswalk.Candidate, len(ms))
	for i, m := range ms {
		out[i] = cand(m.SourceID, m.TargetID, "embedding")
	}
	return out
}

// scoreInput scores profile "synth" (controls s-1, s-2 of source_a) against
// the synthetic target, whose tree is ab-2 > ab-2.3 > ab-2.3.1, cd-1, cd-9
// and top.1.
func scoreInput(refs []crosswalk.Reference, ms []crosswalk.Mapping, cs []crosswalk.Candidate) crosswalk.ScoreInput {
	src := mustCatalog("testdata/source_a.json")
	return crosswalk.ScoreInput{
		Profile: &crosswalk.Profile{
			Name: "synth", Source: "src/a.json", Target: "tgt/synth.json",
			Controls: []string{"s-1", "s-2"}, Digest: "digest-1",
		},
		Source: src,
		Subset: src,
		Target: mustCatalog(syntheticCatalog),
		Refs:   crosswalk.References{Resolved: refs, ResolvedRaw: rawFor(refs)},
		Run: &crosswalk.Run{
			JobID: "job-1", Analysis: json.RawMessage(scoreAnalysis),
			Mappings: ms, Candidates: cs,
		},
		Label:  "nightly",
		Commit: "abc123",
	}
}

// rawFor returns one raw reference per resolved one, as ResolveReferences
// would record them.
func rawFor(refs []crosswalk.Reference) []crosswalk.RawReference {
	out := make([]crosswalk.RawReference, len(refs))
	for i, r := range refs {
		out[i] = crosswalk.RawReference{SourceID: r.SourceID, Identifier: r.TargetID, Citation: r.Citation}
	}
	return out
}

func mustScore(in crosswalk.ScoreInput) *crosswalk.Report {
	r, err := crosswalk.Score(in)
	Expect(err).NotTo(HaveOccurred())
	return r
}

func rate(hit, total int) crosswalk.Rate {
	r := crosswalk.Rate{Hit: hit, Total: total}
	if total > 0 {
		r.Rate = float64(hit) / float64(total)
	}
	return r
}

var _ = Describe("Score", func() {
	Describe("found exact versus base", func() {
		DescribeTable("credits a mapping to a sibling in the same base tree as base only",
			func(refTarget, mappedTarget string) {
				ms := []crosswalk.Mapping{mapping("s-1", mappedTarget, equivalent, 0.8)}
				r := mustScore(scoreInput([]crosswalk.Reference{ref("s-1", refTarget)}, ms, candidatesFor(ms)))
				Expect(r.Recall.FoundExact).To(Equal(rate(0, 1)))
				Expect(r.Recall.FoundBase).To(Equal(rate(1, 1)))
				Expect(r.References[0].FoundExact).To(BeFalse())
				Expect(r.References[0].FoundBase).To(BeTrue())
			},
			Entry("reference to an enhancement, only the base mapped", "ab-2.3", "ab-2"),
			Entry("reference to the base, only an enhancement mapped", "ab-2", "ab-2.3"),
			Entry("reference to a nested enhancement, only its sibling mapped", "ab-2.3.1", "ab-2.3"),
		)

		It("credits an exact mapping as both exact and base", func() {
			ms := []crosswalk.Mapping{mapping("s-1", "ab-2.3", equivalent, 0.8)}
			r := mustScore(scoreInput([]crosswalk.Reference{ref("s-1", "ab-2.3")}, ms, candidatesFor(ms)))
			Expect(r.Recall.FoundExact).To(Equal(rate(1, 1)))
			Expect(r.Recall.FoundBase).To(Equal(rate(1, 1)))
		})

		It("does not credit a mapping from another source control", func() {
			ms := []crosswalk.Mapping{mapping("s-2", "ab-2", equivalent, 0.8)}
			r := mustScore(scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, ms, candidatesFor(ms)))
			Expect(r.Recall.FoundBase).To(Equal(rate(0, 1)))
		})
	})

	Describe("statement items as sources", func() {
		itemInput := func(refTarget string, ms []crosswalk.Mapping, cs []crosswalk.Candidate) crosswalk.ScoreInput {
			in := scoreInput([]crosswalk.Reference{ref("s-1", refTarget)}, ms, cs)
			in.Subset = mustCatalog(sourceItemsCatalog)
			return in
		}

		DescribeTable("credits an item's mapping to the profile control that contains it",
			func(item, mappedTarget string, foundExact, foundBase int) {
				ms := []crosswalk.Mapping{mapping(item, mappedTarget, equivalent, 0.8)}
				r := mustScore(itemInput("ab-2.3", ms, candidatesFor(ms)))
				Expect(r.Recall.FoundExact).To(Equal(rate(foundExact, 1)))
				Expect(r.Recall.FoundBase).To(Equal(rate(foundBase, 1)))
				Expect(r.Recall.ReachableExact).To(Equal(rate(foundExact, 1)))
				Expect(r.Recall.ReachableBase).To(Equal(rate(foundBase, 1)))
			},
			Entry("item with an _smt part ID, exact target", "s-1.a", "ab-2.3", 1, 1),
			Entry("item numbered by the importer, exact target", "s-1.b", "ab-2.3", 1, 1),
			Entry("item, target in the same base tree", "s-1.a", "ab-2", 0, 1),
			Entry("item, target elsewhere", "s-1.a", "cd-1", 0, 0),
		)

		It("credits an item of another profile control to that control", func() {
			ms := []crosswalk.Mapping{mapping("s-2.2", "ab-2.3", equivalent, 0.8)}
			in := itemInput("ab-2.3", ms, candidatesFor(ms))
			in.Refs.Resolved[0] = ref("s-2", "ab-2.3")
			in.Refs.ResolvedRaw = rawFor(in.Refs.Resolved)
			Expect(mustScore(in).Recall.FoundExact).To(Equal(rate(1, 1)))
		})

		It("counts two items of one control mapping one target once, without a duplicate error", func() {
			ms := []crosswalk.Mapping{
				mapping("s-1.a", "ab-2.3", equivalent, 0.8),
				mapping("s-1.b", "ab-2.3", subsetOf, 0.6),
			}
			r := mustScore(itemInput("ab-2.3", ms, candidatesFor(ms)))
			Expect(r.Recall.FoundExact).To(Equal(rate(1, 1)))
			Expect(r.Related).To(HaveLen(2))
		})

		It("still requires each mapping to be a candidate as exported", func() {
			ms := []crosswalk.Mapping{mapping("s-1.a", "ab-2.3", equivalent, 0.8)}
			cs := []crosswalk.Candidate{cand("s-1.b", "ab-2.3", "embedding")}
			_, err := crosswalk.Score(itemInput("ab-2.3", ms, cs))
			Expect(err).To(MatchError(And(ContainSubstring("s-1.a -> ab-2.3"), ContainSubstring("not a candidate"))))
		})

		It("still rejects two identical exported mappings", func() {
			ms := []crosswalk.Mapping{mapping("s-1.a", "ab-2.3", equivalent, 0.8), mapping("s-1.a", "ab-2.3", subsetOf, 0.6)}
			_, err := crosswalk.Score(itemInput("ab-2.3", ms, candidatesFor(ms[:1])))
			Expect(err).To(MatchError(ContainSubstring("s-1.a -> ab-2.3 appears 2 times")))
		})

		DescribeTable("rejects a source that is neither a profile control nor one of its items, naming it",
			func(source string) {
				cs := []crosswalk.Candidate{cand(source, "ab-2.3", "embedding")}
				_, err := crosswalk.Score(itemInput("ab-2.3", nil, cs))
				Expect(err).To(MatchError(And(ContainSubstring(`"`+source+`"`), ContainSubstring("statement item"), ContainSubstring("synth"))))
			},
			Entry("an item of a control outside the profile", "s-9.a"),
			Entry("a control outside the profile", "s-9"),
			Entry("an item the importer does not derive", "s-1.z"),
			Entry("a raw part ID", "s-1_smt.a"),
		)
	})

	Describe("statement items as targets", func() {
		itemInput := func(refTarget, mappedTarget string) crosswalk.ScoreInput {
			ms := []crosswalk.Mapping{mapping("s-1", mappedTarget, equivalent, 0.8)}
			in := scoreInput([]crosswalk.Reference{ref("s-1", refTarget)}, ms, candidatesFor(ms))
			in.Target = mustCatalog(statementCatalog)
			return in
		}

		DescribeTable("credits an item's mapping to the control that owns it",
			func(mappedTarget string, foundExact, foundBase int) {
				r := mustScore(itemInput("sa-1", mappedTarget))
				Expect(r.Recall.FoundExact).To(Equal(rate(foundExact, 1)))
				Expect(r.Recall.FoundBase).To(Equal(rate(foundBase, 1)))
				Expect(r.Recall.ReachableExact).To(Equal(rate(foundExact, 1)))
				Expect(r.Recall.ReachableBase).To(Equal(rate(foundBase, 1)))
			},
			Entry("a top-level item of the referenced control", "sa-1.a", 1, 1),
			Entry("a nested item of the referenced control", "sa-1.b.1", 1, 1),
			Entry("the referenced control itself", "sa-1", 1, 1),
		)

		It("credits an item of an enhancement to the enhancement for exact and to its base control for base", func() {
			in := itemInput("sa-1", "sa-1.2.a")
			r := mustScore(in)
			Expect(r.Recall.FoundExact).To(Equal(rate(0, 1)))
			Expect(r.Recall.FoundBase).To(Equal(rate(1, 1)))
			in = itemInput("sa-1.2", "sa-1.2.a")
			Expect(mustScore(in).Recall.FoundExact).To(Equal(rate(1, 1)))
		})

		It("keeps the raw item pair for the candidate check", func() {
			ms := []crosswalk.Mapping{mapping("s-1", "sa-1.a", equivalent, 0.8)}
			in := scoreInput([]crosswalk.Reference{ref("s-1", "sa-1")}, ms, []crosswalk.Candidate{cand("s-1", "sa-1.b.1", "embedding")})
			in.Target = mustCatalog(statementCatalog)
			_, err := crosswalk.Score(in)
			Expect(err).To(MatchError(And(ContainSubstring("s-1 -> sa-1.a"), ContainSubstring("not a candidate"))))
		})

		It("counts two items of one target control once, without a duplicate error", func() {
			ms := []crosswalk.Mapping{mapping("s-1", "sa-1.a", equivalent, 0.8), mapping("s-1", "sa-1.b.1", subsetOf, 0.6)}
			in := scoreInput([]crosswalk.Reference{ref("s-1", "sa-1")}, ms, candidatesFor(ms))
			in.Target = mustCatalog(statementCatalog)
			r := mustScore(in)
			Expect(r.Recall.FoundExact).To(Equal(rate(1, 1)))
			Expect(r.Related).To(HaveLen(2))
		})

		It("still rejects an ID that is neither a control nor an item, naming it", func() {
			_, err := crosswalk.Score(itemInput("sa-1", "sa-1.z"))
			Expect(err).To(MatchError(And(ContainSubstring(`"sa-1.z"`), ContainSubstring(statementCatalog))))
		})
	})

	It("counts a reference as reachable when a candidate has it, even when no mapping does", func() {
		refs := []crosswalk.Reference{ref("s-1", "cd-1"), ref("s-1", "ab-2.3"), ref("s-2", "top.1")}
		ms := []crosswalk.Mapping{mapping("s-1", "cd-1", equivalent, 0.9)}
		cs := []crosswalk.Candidate{cand("s-1", "cd-1", "embedding"), cand("s-1", "ab-2", "embedding")}
		r := mustScore(scoreInput(refs, ms, cs))
		Expect(r.Recall.FoundExact).To(Equal(rate(1, 3)))
		Expect(r.Recall.ReachableExact).To(Equal(rate(1, 3)))
		Expect(r.Recall.ReachableBase).To(Equal(rate(2, 3)))
		Expect(r.References[1]).To(Equal(crosswalk.ScoredReference{
			SourceID: "s-1", TargetID: "ab-2.3", Citation: "cite ab-2.3", ReachableBase: true,
		}))
	})

	It("breaks reachability down per generator, with unattributed for candidates without contributions", func() {
		refs := []crosswalk.Reference{ref("s-1", "cd-1"), ref("s-2", "ab-2"), ref("s-2", "top.1")}
		cs := []crosswalk.Candidate{
			cand("s-1", "cd-1", "embedding"),
			cand("s-2", "ab-2.3", "lexical", "embedding"),
			cand("s-2", "top.1"),
		}
		r := mustScore(scoreInput(refs, nil, cs))
		Expect(r.Recall.ByGenerator).To(Equal(map[string]crosswalk.Reachability{
			"embedding":    {Exact: rate(1, 3), Base: rate(2, 3)},
			"lexical":      {Exact: rate(0, 3), Base: rate(1, 3)},
			"unattributed": {Exact: rate(1, 3), Base: rate(1, 3)},
		}))
	})

	It("lists unresolved and ambiguous references and keeps them out of the denominators", func() {
		in := scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, nil, nil)
		in.Refs.Unresolved = []crosswalk.RawReference{{SourceID: "s-1", Identifier: "ZZ-404", Citation: "c1"}}
		in.Refs.Ambiguous = []crosswalk.RawReference{{SourceID: "s-2", Identifier: "shared-9", Citation: "c2"}}
		r := mustScore(in)
		Expect(r.Resolution).To(Equal(rate(1, 3)))
		Expect(r.Recall.FoundExact.Total).To(Equal(1))
		Expect(r.Recall.ReachableBase.Total).To(Equal(1))
		Expect(r.Unresolved).To(Equal([]crosswalk.UnresolvedReference{
			{SourceID: "s-1", Identifier: "ZZ-404", Citation: "c1"},
		}))
		Expect(r.Ambiguous).To(Equal([]crosswalk.UnresolvedReference{
			{SourceID: "s-2", Identifier: "shared-9", Citation: "c2", Candidates: []string{"cd-1", "cd-9"}},
		}))
	})

	It("counts distinct (source, identifier) references in every state for the resolution rate", func() {
		in := scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, nil, nil)
		in.Refs.ResolvedRaw = []crosswalk.RawReference{
			{SourceID: "s-1", Identifier: "AB-2"}, {SourceID: "s-1", Identifier: "ab-02"}, {SourceID: "s-1", Identifier: "AB-2"},
		}
		in.Refs.Unresolved = []crosswalk.RawReference{
			{SourceID: "s-1", Identifier: "ZZ-404", Citation: "c1"}, {SourceID: "s-1", Identifier: "ZZ-404", Citation: "c2"},
			{SourceID: "s-2", Identifier: "ZZ-404", Citation: "c3"},
		}
		in.Refs.Ambiguous = []crosswalk.RawReference{
			{SourceID: "s-2", Identifier: "shared-9", Citation: "c4"}, {SourceID: "s-2", Identifier: "shared-9", Citation: "c5"},
		}
		r := mustScore(in)
		Expect(r.Resolution).To(Equal(rate(2, 5)))
		Expect(r.Unresolved).To(Equal([]crosswalk.UnresolvedReference{
			{SourceID: "s-1", Identifier: "ZZ-404", Citation: "c1"},
			{SourceID: "s-2", Identifier: "ZZ-404", Citation: "c3"},
		}))
		Expect(r.Ambiguous).To(HaveLen(1))
		Expect(r.Markdown()).To(ContainSubstring("Resolution: 2/5 (40.0%) of distinct (source control, identifier) references resolve to exactly one target control."))
	})

	It("rejects a profile with no resolved references, pointing at discover", func() {
		in := scoreInput(nil, nil, nil)
		in.Refs.Unresolved = []crosswalk.RawReference{{SourceID: "s-1", Identifier: "ZZ-404"}}
		_, err := crosswalk.Score(in)
		Expect(err).To(MatchError("profile synth extracted no references from src/a.json; run " +
			"`task eval:crosswalk:discover SOURCE=src/a.json TARGET=tgt/synth.json` to see which cited resources resolve"))
	})

	Describe("distributions", func() {
		It("counts types, buckets confidence with 1.0 in the last bucket, and takes the median", func() {
			ms := []crosswalk.Mapping{
				mapping("s-1", "ab-2", equivalent, 0.0),
				mapping("s-1", "ab-2.3", equivalent, 0.1),
				mapping("s-1", "cd-1", subsetOf, 0.7),
				mapping("s-2", "cd-9", "RELATIONSHIP_TYPE_UNSPECIFIED", 0.95),
				mapping("s-2", "top.1", subsetOf, 1.0),
				mapping("s-2", "ab-2", equivalent, 0.3),
			}
			r := mustScore(scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, ms, candidatesFor(ms)))
			Expect(r.Distribution.Types).To(Equal(map[string]int{
				equivalent: 3, subsetOf: 2, "RELATIONSHIP_TYPE_UNSPECIFIED": 1,
			}))
			Expect(r.Distribution.Confidence).To(Equal([10]int{1, 1, 0, 1, 0, 0, 0, 1, 0, 2}))
			Expect(r.Distribution.MedianConfidence).To(HaveValue(BeNumerically("~", 0.5)))
		})

		DescribeTable("puts a confidence on a bucket boundary in the bucket it opens",
			func(confidence float64, bucket int) {
				ms := []crosswalk.Mapping{mapping("s-1", "ab-2", equivalent, confidence)}
				r := mustScore(scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, ms, candidatesFor(ms)))
				var want [10]int
				want[bucket] = 1
				Expect(r.Distribution.Confidence).To(Equal(want))
			},
			Entry("0.2", 0.2, 2), Entry("0.3", 0.3, 3), Entry("0.6", 0.6, 6),
			Entry("0.7", 0.7, 7), Entry("0.9", 0.9, 9), Entry("just below 0.1", 0.0999, 0),
		)

		It("has a null median and empty buckets with no related mappings", func() {
			r := mustScore(scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, nil, nil))
			Expect(r.Distribution.MedianConfidence).To(BeNil())
			Expect(r.Distribution.Confidence).To(Equal([10]int{}))
		})
	})

	Describe("cost", func() {
		t0 := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)

		It("reports each stage's wall time and the total from the first start to the last completion", func() {
			in := scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, nil, nil)
			in.Run.Stages = []crosswalk.Stage{
				{Name: "relationship", Status: "JOB_STATUS_COMPLETED", Started: t0.Add(30 * time.Second), Completed: t0.Add(120500 * time.Millisecond)},
				{Name: "embedding", Status: "JOB_STATUS_COMPLETED", Started: t0, Completed: t0.Add(40 * time.Second)},
			}
			r := mustScore(in)
			Expect(r.Cost.Stages).To(HaveLen(2))
			Expect(r.Cost.Stages[0].Seconds).To(HaveValue(BeNumerically("~", 90.5)))
			Expect(r.Cost.Stages[1].Seconds).To(HaveValue(BeNumerically("~", 40)))
			Expect(r.Cost.TotalSeconds).To(HaveValue(BeNumerically("~", 120.5)))
		})

		DescribeTable("reports a stage without both timestamps as incomplete, not 0",
			func(started, completed time.Time) {
				in := scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, nil, nil)
				in.Run.Stages = []crosswalk.Stage{
					{Name: "embedding", Status: "JOB_STATUS_COMPLETED", Started: t0, Completed: t0.Add(time.Second)},
					{Name: "relationship", Status: "JOB_STATUS_RUNNING", Started: started, Completed: completed},
				}
				r := mustScore(in)
				Expect(r.Cost.Stages[1].Seconds).To(BeNil())
				Expect(r.Cost.TotalSeconds).To(BeNil())
				Expect(r.Markdown()).To(ContainSubstring("| relationship | JOB_STATUS_RUNNING | incomplete |"))
			},
			Entry("started, never completed", t0, time.Time{}),
			Entry("never started", time.Time{}, time.Time{}),
		)

		It("has no total without stages", func() {
			r := mustScore(scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, nil, nil))
			Expect(r.Cost.TotalSeconds).To(BeNil())
		})

		It("copies analyzer costs", func() {
			in := scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, nil, nil)
			in.Run.Costs = []crosswalk.AnalyzerCost{
				{Analyzer: "relationship", Mode: "enabled", Attestation: "attested", Tasks: 4, Hits: 3, Dispatched: 1, Comparable: true, Models: []string{"m-a"}},
				{Analyzer: "summary", Mode: "disabled", Attestation: "attested", Tasks: 4, Models: []string{"m-b"}},
			}
			Expect(mustScore(in).Cost.Analyzers).To(Equal(in.Run.Costs))
		})

		DescribeTable("reports how many relationship results came from earlier jobs",
			func(costs []crosswalk.AnalyzerCost, want *crosswalk.Reuse, line string) {
				in := scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, nil, nil)
				in.Run.Costs = costs
				r := mustScore(in)
				Expect(r.Cost.RelationshipReuse).To(Equal(want))
				Expect(r.Markdown()).To(ContainSubstring(line))
			},
			Entry("relationship analyzer present", []crosswalk.AnalyzerCost{
				{Analyzer: "classify", Mode: "enabled", Attestation: "attested", Tasks: 50, Hits: 50, Comparable: true},
				{Analyzer: "relationship", Mode: "enabled", Attestation: "attested", Tasks: 300, Hits: 12, Dispatched: 288, Comparable: true},
			}, &crosswalk.Reuse{Hits: 12, Tasks: 300}, "relationship reused 12 of 300 results from earlier jobs"),
			Entry("relationship analyzer absent", []crosswalk.AnalyzerCost{
				{Analyzer: "classify", Mode: "enabled", Attestation: "attested", Tasks: 50, Hits: 50, Comparable: true},
			}, nil, "no relationship analyzer in this job"),
			Entry("name matched exactly", []crosswalk.AnalyzerCost{
				{Analyzer: "Relationship", Mode: "enabled", Attestation: "attested", Tasks: 3, Hits: 1, Comparable: true},
			}, nil, "no relationship analyzer in this job"),
		)
	})

	Describe("input checks", func() {
		It("rejects a related mapping that is not a candidate, naming the pair", func() {
			ms := []crosswalk.Mapping{mapping("s-1", "ab-2", equivalent, 0.5), mapping("s-2", "cd-1", equivalent, 0.5)}
			_, err := crosswalk.Score(scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, ms, candidatesFor(ms[:1])))
			Expect(err).To(MatchError(And(
				ContainSubstring("s-2 -> cd-1"),
				ContainSubstring("not a candidate"),
				ContainSubstring("same job"),
			)))
		})

		DescribeTable("rejects a mapping or candidate outside the profile's controls, naming it",
			func(ms []crosswalk.Mapping, cs []crosswalk.Candidate, want string) {
				_, err := crosswalk.Score(scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, ms, cs))
				Expect(err).To(MatchError(And(ContainSubstring(want), ContainSubstring("synth"))))
			},
			Entry("mapping source", []crosswalk.Mapping{mapping("s-3", "ab-2", equivalent, 0.5)},
				[]crosswalk.Candidate{cand("s-3", "ab-2")}, `"s-3"`),
			Entry("candidate source", nil, []crosswalk.Candidate{cand("s-9", "ab-2")}, `"s-9"`),
		)

		DescribeTable("rejects a mapping or candidate whose target is not in the target catalog",
			func(ms []crosswalk.Mapping, cs []crosswalk.Candidate) {
				_, err := crosswalk.Score(scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, ms, cs))
				Expect(err).To(MatchError(And(ContainSubstring(`"zz-1"`), ContainSubstring(syntheticCatalog))))
			},
			Entry("mapping target", []crosswalk.Mapping{mapping("s-1", "zz-1", equivalent, 0.5)},
				[]crosswalk.Candidate{cand("s-1", "zz-1")}),
			Entry("candidate target", nil, []crosswalk.Candidate{cand("s-1", "zz-1")}),
		)

		It("rejects two related mappings for one pair", func() {
			ms := []crosswalk.Mapping{mapping("s-1", "ab-2", equivalent, 0.5), mapping("s-1", "ab-2", subsetOf, 0.4)}
			_, err := crosswalk.Score(scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, ms, candidatesFor(ms[:1])))
			Expect(err).To(MatchError(ContainSubstring("s-1 -> ab-2 appears 2 times")))
		})

		DescribeTable("rejects references the profile could not have produced",
			func(r crosswalk.Reference, want string) {
				_, err := crosswalk.Score(scoreInput([]crosswalk.Reference{r}, nil, nil))
				Expect(err).To(MatchError(ContainSubstring(want)))
			},
			Entry("source outside the controls", ref("s-3", "ab-2"), `"s-3"`),
			Entry("target outside the target catalog", ref("s-1", "zz-1"), `"zz-1"`),
		)

		It("rejects a bidirectional job", func() {
			in := scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, nil, nil)
			in.Run.Analysis = json.RawMessage(`{"sourceCatalog":{"catalogId":"cat-src"},"targetCatalog":{"catalogId":"cat-tgt"},"bidirectional":true}`)
			_, err := crosswalk.Score(in)
			Expect(err).To(MatchError("job job-1 is bidirectional; the eval scores one direction (source → target). Start the job without --bidirectional."))
		})

		It("rejects a run without the export's analysis", func() {
			in := scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, nil, nil)
			in.Run.Analysis = json.RawMessage("null")
			_, err := crosswalk.Score(in)
			Expect(err).To(MatchError(And(ContainSubstring("job-1"), ContainSubstring("analysis"))))
		})
	})

	It("records provenance", func() {
		in := scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, nil, nil)
		r := mustScore(in)
		Expect(r.Provenance).To(Equal(crosswalk.Provenance{
			Label: "nightly", Commit: "abc123", JobID: "job-1",
			SourceCatalogID: "cat-src", TargetCatalogID: "cat-tgt",
			Profile:      crosswalk.ProfileIdentity{Name: "synth", Digest: "digest-1", Controls: []string{"s-1", "s-2"}},
			SourceDigest: in.Source.Digest, SubsetDigest: in.Subset.Digest, TargetDigest: in.Target.Digest,
		}))
	})

	It("reports zero rates with totals, null medians and no NaN or Inf for empty results", func() {
		in := scoreInput([]crosswalk.Reference{ref("s-1", "ab-2"), ref("s-2", "cd-1")}, nil, nil)
		in.Baseline = mustScore(scoreInput([]crosswalk.Reference{ref("s-1", "ab-2"), ref("s-2", "cd-1")}, nil, nil))
		r := mustScore(in)
		Expect(r.Recall.FoundExact).To(Equal(crosswalk.Rate{Hit: 0, Total: 2, Rate: 0}))
		Expect(r.Recall.ReachableBase).To(Equal(crosswalk.Rate{Hit: 0, Total: 2, Rate: 0}))
		Expect(r.Agreement.Jaccard).To(Equal(crosswalk.Rate{}))
		Expect(r.Agreement.SameType).To(Equal(crosswalk.Rate{}))
		Expect(r.Agreement.MedianAbsConfidenceDelta).To(BeNil())
		raw, err := json.Marshal(r)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).NotTo(Or(ContainSubstring("NaN"), ContainSubstring("Inf")))
		Expect(string(raw)).To(ContainSubstring(`"median_confidence":null`))
		Expect(r.Markdown()).NotTo(Or(ContainSubstring("NaN"), ContainSubstring("Inf")))
	})

	Describe("agreement with a baseline", func() {
		refs := []crosswalk.Reference{ref("s-1", "ab-2")}

		It("compares the related pair sets, their types and confidences", func() {
			prev := []crosswalk.Mapping{
				mapping("s-1", "ab-2", equivalent, 0.9),
				mapping("s-1", "cd-1", equivalent, 0.5),
				mapping("s-2", "top.1", subsetOf, 0.2),
				mapping("s-2", "cd-9", equivalent, 0.6),
			}
			cur := []crosswalk.Mapping{
				mapping("s-1", "ab-2", equivalent, 0.8),
				mapping("s-1", "cd-1", subsetOf, 0.9),
				mapping("s-2", "top.1", subsetOf, 0.2),
				mapping("s-2", "ab-2", equivalent, 0.7),
			}
			baseIn := scoreInput(refs, prev, candidatesFor(prev))
			baseIn.Label, baseIn.Run.JobID, baseIn.Commit = "before", "job-0", "000aaa"
			in := scoreInput(refs, cur, candidatesFor(cur))
			in.Baseline = mustScore(baseIn)
			a := mustScore(in).Agreement
			Expect(a).NotTo(BeNil())
			Expect(a.BaselineLabel).To(Equal("before"))
			Expect(a.BaselineJobID).To(Equal("job-0"))
			Expect(a.BaselineCommit).To(Equal("000aaa"))
			Expect(a.Both).To(Equal(3))
			Expect(a.OnlyCurrent).To(Equal([]crosswalk.Pair{{SourceID: "s-2", TargetID: "ab-2"}}))
			Expect(a.OnlyBaseline).To(Equal([]crosswalk.Pair{{SourceID: "s-2", TargetID: "cd-9"}}))
			Expect(a.Jaccard).To(Equal(rate(3, 5)))
			Expect(a.SameType).To(Equal(rate(2, 3)))
			// |Δ| over shared pairs: 0.1, 0.4, 0.0 -> median 0.1.
			Expect(a.MedianAbsConfidenceDelta).To(HaveValue(BeNumerically("~", 0.1, 1e-9)))
		})

		DescribeTable("labels agreement not independent when relationship results were reused",
			func(hits int64, independent bool) {
				in := scoreInput(refs, nil, nil)
				in.Run.Costs = []crosswalk.AnalyzerCost{{Analyzer: "relationship", Mode: "enabled", Attestation: "attested", Tasks: 300, Hits: hits, Comparable: true}}
				in.Baseline = mustScore(scoreInput(refs, nil, nil))
				r := mustScore(in)
				Expect(r.Agreement.Independent).To(HaveValue(Equal(independent)))
				Expect(r.Agreement.ReusedRelationshipResults).To(Equal(hits))
				label := fmt.Sprintf("not independent: %d relationship results were reused from earlier jobs; rerun with NO_CACHE=1 to measure run-to-run variance", hits)
				if independent {
					Expect(r.Markdown()).NotTo(ContainSubstring("not independent"))
				} else {
					Expect(r.Markdown()).To(ContainSubstring(label))
				}
			},
			Entry("hits above 0", int64(12), false),
			Entry("no hits", int64(0), true),
		)

		DescribeTable("reports independence unknown unless the relationship cost is comparable",
			func(costs []crosswalk.AnalyzerCost) {
				in := scoreInput(refs, nil, nil)
				in.Run.Costs = costs
				in.Baseline = mustScore(scoreInput(refs, nil, nil))
				r := mustScore(in)
				Expect(r.Agreement.Independent).To(BeNil())
				Expect(r.Agreement.ReusedRelationshipResults).To(BeZero())
				Expect(r.Markdown()).To(ContainSubstring("independence unknown"))
				Expect(r.Markdown()).NotTo(ContainSubstring("not independent"))
			},
			Entry("no relationship analyzer", nil),
			Entry("unattested summary", []crosswalk.AnalyzerCost{{Analyzer: "relationship", Mode: "enabled", Attestation: "unattested", Tasks: 3}}),
			Entry("disabled mode", []crosswalk.AnalyzerCost{{Analyzer: "relationship", Mode: "disabled", Attestation: "attested", Tasks: 3}}),
			Entry("hits in a non-comparable summary", []crosswalk.AnalyzerCost{{Analyzer: "relationship", Mode: "skipped", Attestation: "attested", Tasks: 3, Hits: 3}}),
		)

		DescribeTable("sums chat calls without embedding, and none unless every analyzer is comparable",
			func(costs []crosswalk.AnalyzerCost, want int64, ok bool) {
				got, gotOK := crosswalk.Cost{Analyzers: costs}.ChatCalls()
				Expect(gotOK).To(Equal(ok))
				Expect(got).To(Equal(want))
			},
			Entry("embedding excluded", []crosswalk.AnalyzerCost{
				{Analyzer: "classify", Dispatched: 10, Comparable: true},
				{Analyzer: "embedding", Dispatched: 10, Comparable: true},
				{Analyzer: "relationship", Dispatched: 5, Comparable: true},
			}, int64(15), true),
			Entry("no analyzers", nil, int64(0), false),
			Entry("one analyzer not comparable", []crosswalk.AnalyzerCost{
				{Analyzer: "classify", Dispatched: 10, Comparable: true},
				{Analyzer: "relationship", Dispatched: 5},
			}, int64(0), false),
			Entry("embedding not comparable", []crosswalk.AnalyzerCost{
				{Analyzer: "classify", Dispatched: 10, Comparable: true},
				{Analyzer: "embedding", Dispatched: 10},
			}, int64(0), false),
		)

		It("includes each model's deployment hash in the report", func() {
			files := writeRunFiles(GinkgoT().TempDir(), "job-7", "job-7", "job-7", "bypassed", "attested")
			raw, err := os.ReadFile(files.status)
			Expect(err).NotTo(HaveOccurred())
			writeFile(files.status, strings.Replace(string(raw), `{"alias": "a", "observed_model": "model-a", "deployment": ""`, `{"alias": "a", "observed_model": "model-a", "deployment": "dep-a"`, 1))
			r, err := crosswalk.LoadRun(files.mappings, files.candidates, files.status)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Costs[0].Models).To(Equal([]string{"model-a (deployment dep-a)", "model-b", "model-a"}))
		})

		It("is absent without a baseline", func() {
			Expect(mustScore(scoreInput(refs, nil, nil)).Agreement).To(BeNil())
		})

		DescribeTable("rejects a baseline scored with another profile, naming the profile and the first difference",
			func(change func(p *crosswalk.Profile, refs *[]crosswalk.Reference), want string) {
				baseRefs := []crosswalk.Reference{ref("s-1", "ab-2"), ref("s-2", "cd-1")}
				baseIn := scoreInput(baseRefs, nil, nil)
				baseline := mustScore(baseIn)

				curRefs := []crosswalk.Reference{ref("s-1", "ab-2"), ref("s-2", "cd-1")}
				in := scoreInput(curRefs, nil, nil)
				in.Profile.Digest = "digest-2"
				change(in.Profile, &in.Refs.Resolved)
				in.Baseline = baseline
				_, err := crosswalk.Score(in)
				Expect(err).To(MatchError(And(
					ContainSubstring("profile synth"),
					ContainSubstring("digest-1"),
					ContainSubstring("digest-2"),
					ContainSubstring(want),
				)))
			},
			Entry("a control differs", func(p *crosswalk.Profile, _ *[]crosswalk.Reference) {
				p.Controls = []string{"s-2", "s-1"}
			}, `control 1 is "s-1" in the baseline and "s-2" now`),
			Entry("a control is added", func(p *crosswalk.Profile, _ *[]crosswalk.Reference) {
				p.Controls = []string{"s-1", "s-2", "s-3"}
			}, `control 3 is (none) in the baseline and "s-3" now`),
			Entry("a reference differs", func(_ *crosswalk.Profile, refs *[]crosswalk.Reference) {
				(*refs)[1] = ref("s-2", "cd-9")
			}, `reference 2 is s-2 -> cd-1 in the baseline and s-2 -> cd-9 now`),
			Entry("a reference is dropped", func(_ *crosswalk.Profile, refs *[]crosswalk.Reference) {
				*refs = (*refs)[:1]
			}, `reference 2 is s-2 -> cd-1 in the baseline and (none) now`),
			Entry("only the file differs", func(*crosswalk.Profile, *[]crosswalk.Reference) {},
				"controls and resolved references match"),
		)

		Describe("with a references.csv yardstick", func() {
			csvInput := func(digest string) crosswalk.ScoreInput {
				in := scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, nil, nil)
				in.Profile.References.CSV = "refs.csv"
				in.CSVDigest = digest
				return in
			}

			It("names the CSV digest in the report", func() {
				r := mustScore(csvInput("csv-digest-1"))
				Expect(r.Provenance.Profile.CSVDigest).To(Equal("csv-digest-1"))
				Expect(r.Markdown()).To(ContainSubstring("- References CSV SHA-256: csv-digest-1\n"))
			})

			It("leaves the CSV line out for a profile without one", func() {
				Expect(mustScore(scoreInput(refs, nil, nil)).Markdown()).NotTo(ContainSubstring("References CSV"))
			})

			It("refuses to score without the CSV digest", func() {
				_, err := crosswalk.Score(csvInput(""))
				Expect(err).To(MatchError(And(ContainSubstring("refs.csv"), ContainSubstring("CSVDigest"))))
			})

			It("rejects a baseline scored with another CSV under the same profile file, naming both digests", func() {
				in := csvInput("csv-digest-2")
				in.Baseline = mustScore(csvInput("csv-digest-1"))
				_, err := crosswalk.Score(in)
				Expect(err).To(MatchError(And(ContainSubstring("references.csv"), ContainSubstring("csv-digest-1"), ContainSubstring("csv-digest-2"))))
			})

			It("accepts a baseline scored with the same CSV", func() {
				in := csvInput("csv-digest-1")
				in.Baseline = mustScore(csvInput("csv-digest-1"))
				Expect(mustScore(in).Agreement).NotTo(BeNil())
			})
		})
	})
})

var _ = Describe("LoadRun", func() {
	It("reads related mappings, candidates, stages and cache costs, dropping NO_RELATIONSHIP", func() {
		files := writeRunFiles(GinkgoT().TempDir(), "job-7", "job-7", "job-7", "bypassed", "attested")
		r, err := crosswalk.LoadRun(files.mappings, files.candidates, files.status)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.JobID).To(Equal("job-7"))
		Expect(r.Analysis).To(MatchJSON(scoreAnalysis))
		Expect(r.Mappings).To(Equal([]crosswalk.Mapping{
			mapping("s-1", "ab-2", equivalent, 0.75),
			mapping("s-2", "cd-1", "RELATIONSHIP_TYPE_UNSPECIFIED", 0),
		}))
		Expect(r.Candidates).To(Equal([]crosswalk.Candidate{
			cand("s-1", "ab-2", "embedding", "lexical"),
			cand("s-2", "cd-1"),
			cand("s-2", "top.1", "lexical"),
		}))
		Expect(r.Stages).To(Equal([]crosswalk.Stage{
			{Name: "embedding", Status: "JOB_STATUS_COMPLETED",
				Started:   time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC),
				Completed: time.Date(2026, 11, 8, 10, 0, 1, 500_000_000, time.UTC)},
			{Name: "relationship", Status: "JOB_STATUS_PENDING"},
		}))
		Expect(r.Costs).To(Equal([]crosswalk.AnalyzerCost{{
			Analyzer: "relationship", Mode: "bypassed", Attestation: "attested", Tasks: 3, Hits: 0, Dispatched: 9,
			Comparable: true, Models: []string{"model-a", "model-b"},
		}}))
	})

	DescribeTable("counts dispatched only for an attested run that sent its calls (cache enabled or bypassed)",
		func(mode, attestation string, comparable bool, cell string) {
			files := writeRunFiles(GinkgoT().TempDir(), "job-7", "job-7", "job-7", mode, attestation)
			r, err := crosswalk.LoadRun(files.mappings, files.candidates, files.status)
			Expect(err).NotTo(HaveOccurred())
			Expect(r.Costs[0].Comparable).To(Equal(comparable))
			in := scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, nil, nil)
			in.Run = r
			Expect(mustScore(in).Markdown()).To(ContainSubstring(cell))
		},
		Entry("enabled", "enabled", "attested", true, "| relationship | enabled | 3 | 0 | 9 | model-a, model-b |"),
		Entry("bypassed", "bypassed", "attested", true, "| relationship | bypassed | 3 | 0 | 9 | model-a, model-b |"),
		Entry("disabled", "disabled", "attested", false, "| relationship | disabled | 3 | 0 | not comparable: cache mode disabled | model-a, model-b |"),
		Entry("skipped", "skipped", "attested", false, "| relationship | skipped | 3 | 0 | not comparable: cache mode skipped |"),
		Entry("unrecorded", "unrecorded", "attested", false, "| relationship | unrecorded | 3 | 0 | not comparable: cache mode unrecorded |"),
		Entry("no mode", "", "attested", false, "| relationship |  | 3 | 0 | not comparable: cache mode (none) |"),
		Entry("unattested", "bypassed", "unattested", false, "| relationship | bypassed | 3 | 0 | not comparable: unattested |"),
		Entry("attestation in progress", "enabled", "in_progress", false, "| relationship | enabled | 3 | 0 | not comparable: unattested |"),
	)

	DescribeTable("rejects files from different jobs, naming both job IDs",
		func(mJob, cJob, sJob, first, second string) {
			files := writeRunFiles(GinkgoT().TempDir(), mJob, cJob, sJob, "bypassed", "attested")
			_, err := crosswalk.LoadRun(files.mappings, files.candidates, files.status)
			Expect(err).To(MatchError(And(ContainSubstring(first), ContainSubstring(second), ContainSubstring("same job"))))
		},
		Entry("candidates from another job", "job-a", "job-b", "job-a", `"job-a"`, `"job-b"`),
		Entry("status from another job", "job-a", "job-a", "job-c", `"job-a"`, `"job-c"`),
	)

	DescribeTable("rejects malformed input, naming the file",
		func(which, content, want string) {
			dir := GinkgoT().TempDir()
			files := writeRunFiles(dir, "job-7", "job-7", "job-7", "bypassed", "attested")
			path := map[string]string{"mappings": files.mappings, "candidates": files.candidates, "status": files.status}[which]
			writeFile(path, content)
			_, err := crosswalk.LoadRun(files.mappings, files.candidates, files.status)
			Expect(err).To(MatchError(And(ContainSubstring(path), ContainSubstring(want))))
		},
		Entry("mappings not JSON", "mappings", "{", "decoding"),
		Entry("no job_id", "mappings", `{"mappings":[]}`, "job_id"),
		Entry("NaN confidence", "mappings", `{"job_id":"job-7","mappings":[{"sourceControlId":"s-1","targetControlId":"ab-2","relationshipType":"`+equivalent+`","confidence":"NaN"}]}`, "decoding"),
		Entry("confidence above 1", "mappings", `{"job_id":"job-7","mappings":[{"sourceControlId":"s-1","targetControlId":"ab-2","relationshipType":"`+equivalent+`","confidence":1.5}]}`, "between 0 and 1"),
		Entry("negative confidence", "mappings", `{"job_id":"job-7","mappings":[{"sourceControlId":"s-1","targetControlId":"ab-2","relationshipType":"`+equivalent+`","confidence":-0.1}]}`, "between 0 and 1"),
		Entry("bad stage timestamp", "status", `{"job_id":"job-7","stages":[{"name":"embedding","status":"X","started_at":"yesterday","completed_at":""}]}`, "embedding"),
		Entry("candidates not JSON", "candidates", "[]", "decoding"),
	)

	DescribeTable("rejects a node ID that is not the job's catalog ID and a control ID, naming the file, field and ID",
		func(file, field, id, want string) {
			files := writeRunFiles(GinkgoT().TempDir(), "job-7", "job-7", "job-7", "bypassed", "attested")
			path := map[string]string{"mappings": files.mappings, "candidates": files.candidates}[file]
			raw, err := os.ReadFile(path)
			Expect(err).NotTo(HaveOccurred())
			good := map[string]string{"sourceControlId": "cat-src/s-1", "targetControlId": "cat-tgt/ab-2"}[field]
			writeFile(path, strings.Replace(string(raw), `"`+field+`": "`+good+`"`, `"`+field+`": "`+id+`"`, 1))

			_, err = crosswalk.LoadRun(files.mappings, files.candidates, files.status)
			Expect(err).To(MatchError(And(
				ContainSubstring(path), ContainSubstring(field), ContainSubstring(`"`+id+`"`),
				ContainSubstring(want), ContainSubstring("same job"))))
		},
		Entry("bare mapping source", "mappings", "sourceControlId", "s-1", `"cat-src/"`),
		Entry("bare mapping target", "mappings", "targetControlId", "ab-2", `"cat-tgt/"`),
		Entry("bare candidate source", "candidates", "sourceControlId", "s-1", `"cat-src/"`),
		Entry("bare candidate target", "candidates", "targetControlId", "ab-2", `"cat-tgt/"`),
		Entry("mapping source from the target catalog", "mappings", "sourceControlId", "cat-tgt/s-1", `"cat-src/"`),
		Entry("candidate target from another catalog", "candidates", "targetControlId", "other/ab-2", `"cat-tgt/"`),
		Entry("mapping target with nothing after the prefix", "mappings", "targetControlId", "cat-tgt/", `"cat-tgt/"`),
		Entry("candidate source with nothing after the prefix", "candidates", "sourceControlId", "cat-src/", `"cat-src/"`),
	)

	It("keeps a control ID that itself contains a slash", func() {
		files := writeRunFiles(GinkgoT().TempDir(), "job-7", "job-7", "job-7", "bypassed", "attested")
		raw, err := os.ReadFile(files.candidates)
		Expect(err).NotTo(HaveOccurred())
		writeFile(files.candidates, strings.Replace(string(raw), `"cat-tgt/top.1"`, `"cat-tgt/a/b"`, 1))
		r, err := crosswalk.LoadRun(files.mappings, files.candidates, files.status)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.Candidates[2].TargetID).To(Equal("a/b"))
	})

	It("names a missing file", func() {
		files := writeRunFiles(GinkgoT().TempDir(), "job-7", "job-7", "job-7", "bypassed", "attested")
		_, err := crosswalk.LoadRun(files.mappings+".missing", files.candidates, files.status)
		Expect(err).To(MatchError(ContainSubstring(files.mappings + ".missing")))
	})
})

type runFiles struct{ mappings, candidates, status string }

func writeFile(path, content string) {
	Expect(os.WriteFile(path, []byte(content), 0o600)).To(Succeed())
}

// writeRunFiles writes a results export, a candidates export and a run status
// in the shapes the crosscodex CLI prints, extra fields included.
func writeRunFiles(dir, mappingsJob, candidatesJob, statusJob, cacheMode, attestation string) runFiles {
	f := runFiles{
		mappings:   filepath.Join(dir, "mappings.json"),
		candidates: filepath.Join(dir, "candidates.json"),
		status:     filepath.Join(dir, "status.json"),
	}
	writeFile(f.mappings, fmt.Sprintf(`{"job_id": %q, "analysis": %s, "mappings": [
		{"mappingId": "m1", "sourceControlId": "cat-src/s-1", "targetControlId": "cat-tgt/ab-2", "relationshipType": "RELATIONSHIP_TYPE_EQUIVALENT", "confidence": 0.75, "contributions": []},
		{"mappingId": "m2", "sourceControlId": "cat-src/s-1", "targetControlId": "cat-tgt/cd-1", "relationshipType": "RELATIONSHIP_TYPE_NO_RELATIONSHIP", "confidence": 0.9, "contributions": []},
		{"mappingId": "m3", "sourceControlId": "cat-src/s-2", "targetControlId": "cat-tgt/cd-1", "relationshipType": "RELATIONSHIP_TYPE_UNSPECIFIED", "confidence": 0, "contributions": []}
	]}`, mappingsJob, scoreAnalysis))
	writeFile(f.candidates, fmt.Sprintf(`{"job_id": %q, "analysis": %s, "candidates": [
		{"sourceControlId": "cat-src/s-1", "targetControlId": "cat-tgt/ab-2", "similarityScore": 81.5, "contributions": [
			{"generator": "embedding", "score": 0.8, "weight": 1, "metadata": {}},
			{"generator": "lexical", "score": 0.4, "weight": 1, "metadata": {}},
			{"generator": "embedding", "score": 0.8, "weight": 1, "metadata": {}}]},
		{"sourceControlId": "cat-src/s-2", "targetControlId": "cat-tgt/cd-1", "similarityScore": 0, "contributions": []},
		{"sourceControlId": "cat-src/s-2", "targetControlId": "cat-tgt/top.1", "similarityScore": 12, "contributions": [
			{"generator": "lexical", "score": 0.1, "weight": 1, "metadata": {"k": "v"}}]}
	]}`, candidatesJob, scoreAnalysis))
	writeFile(f.status, fmt.Sprintf(`{"job_id": %q, "status": "JOB_STATUS_RUNNING", "analyzers": ["relationship"],
		"stages": [
			{"name": "embedding", "status": "JOB_STATUS_COMPLETED", "started_at": "2026-11-08T10:00:00Z", "completed_at": "2026-11-08T10:00:01.5Z"},
			{"name": "relationship", "status": "JOB_STATUS_PENDING", "started_at": "", "completed_at": ""}
		],
		"cache": [{"analyzer": "relationship", "attestation": %q, "mode": %q, "reason": "", "tasks": 3, "hits": 0, "dispatched": 9,
			"models": [
				{"alias": "a", "observed_model": "model-a", "deployment": "", "revision": ""},
				{"alias": "b", "observed_model": "model-b", "deployment": "", "revision": ""},
				{"alias": "c", "observed_model": "model-a", "deployment": "", "revision": ""},
				{"alias": "d", "observed_model": "", "deployment": "", "revision": ""}],
			"producer_job_ids": [], "other_producer_jobs": 0}]}`, statusJob, attestation, cacheMode))
	return f
}

var _ = Describe("Score precision", func() {
	lbl := func(verdict string) crosswalk.Label {
		return crosswalk.Label{Verdict: verdict, JudgedBy: []string{"opus"}}
	}
	pr := func(s, t string) crosswalk.Pair { return crosswalk.Pair{SourceID: s, TargetID: t} }
	refs := []crosswalk.Reference{ref("s-1", "ab-2"), ref("s-2", "cd-1")}
	rows := []crosswalk.Mapping{
		mapping("s-1", "ab-2", equivalent, 0.8), mapping("s-2", "cd-1", equivalent, 0.8),
		mapping("s-1", "cd-9", equivalent, 0.8), mapping("s-2", "top.1", equivalent, 0.8),
	}
	labels := map[crosswalk.Pair]crosswalk.Label{
		pr("s-1", "ab-2"): lbl("related"), pr("s-2", "cd-1"): lbl("related"),
		pr("s-1", "cd-9"): lbl("unrelated"), pr("s-2", "top.1"): lbl("disputed"),
	}

	score := func(refs []crosswalk.Reference, ms []crosswalk.Mapping, ls map[crosswalk.Pair]crosswalk.Label) crosswalk.Precision {
		in := scoreInput(refs, ms, candidatesFor(ms))
		in.Labels = ls
		return mustScore(in).Precision
	}

	It("reports reference, labelled and coverage rates and lists disputed rows", func() {
		p := score(refs, rows, labels)
		Expect(p.Reference).To(Equal(rate(2, 4)))
		Expect(p.Labelled).To(Equal(&crosswalk.Rate{Hit: 2, Total: 3, Rate: 2.0 / 3}))
		Expect(p.Coverage).To(Equal(&crosswalk.Rate{Hit: 3, Total: 4, Rate: 0.75}))
		Expect(p.Disputed).To(Equal([]crosswalk.Pair{pr("s-2", "top.1")}))
		Expect(p.Unlabelled).To(BeEmpty())
	})

	It("lists rows without a label, sorted, and leaves them out of the labelled rate", func() {
		ls := map[crosswalk.Pair]crosswalk.Label{pr("s-1", "ab-2"): lbl("related"), pr("s-1", "cd-9"): lbl("unrelated")}
		p := score(refs, rows, ls)
		Expect(p.Labelled).To(Equal(&crosswalk.Rate{Hit: 1, Total: 2, Rate: 0.5}))
		Expect(p.Coverage).To(Equal(&crosswalk.Rate{Hit: 2, Total: 4, Rate: 0.5}))
		Expect(p.Unlabelled).To(Equal([]crosswalk.Pair{pr("s-2", "cd-1"), pr("s-2", "top.1")}))
		Expect(p.Disputed).To(BeEmpty())
	})

	It("has no labelled rate or coverage without labels", func() {
		p := score(refs, rows, nil)
		Expect(p.Reference).To(Equal(rate(2, 4)))
		Expect(p.Labelled).To(BeNil())
		Expect(p.Coverage).To(BeNil())
		Expect(p.Unlabelled).To(BeEmpty())
	})

	It("counts a row on a sibling in the reference's base tree as agreeing", func() {
		ms := []crosswalk.Mapping{mapping("s-1", "ab-2.3", equivalent, 0.8), mapping("s-2", "ab-2.3", equivalent, 0.8)}
		Expect(score(refs[:1], ms, nil).Reference).To(Equal(rate(1, 2)))
	})

	It("ignores labels for pairs the run did not produce", func() {
		ls := map[crosswalk.Pair]crosswalk.Label{pr("s-2", "cd-9"): lbl("related")}
		p := score(refs, rows, ls)
		Expect(p.Labelled).To(Equal(&crosswalk.Rate{}))
		Expect(p.Coverage).To(Equal(&crosswalk.Rate{Total: 4}))
		Expect(p.Unlabelled).To(HaveLen(4))
	})

	It("reports zero rates, not NaN, for a run with no rows", func() {
		p := score(refs, nil, labels)
		Expect(p.Reference).To(Equal(rate(0, 0)))
		Expect(p.Coverage).To(Equal(&crosswalk.Rate{}))
	})
})
