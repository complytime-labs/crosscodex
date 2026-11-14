package results_test

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
)

// critiqueOf builds a critique of one pair; specs override only the fields
// under test.
func critiqueOf(kind, src, tgt, outcome string, upheld, rejected, failed int, reasons ...string) results.CritiqueResult {
	return results.CritiqueResult{Kind: kind, SourceControlID: src, TargetControlID: tgt,
		Upheld: upheld, Rejected: rejected, Failed: failed, Outcome: outcome,
		Vetoed: outcome == results.CritiqueVetoed, Reasons: reasons}
}

var _ = Describe("critique verdicts", func() {
	It("names the edge properties and the deferral metadata key the pipeline and the graph share", func() {
		Expect([]string{results.CritiqueUpheldProperty, results.CritiqueRejectedProperty, results.CritiqueNoteProperty}).
			To(Equal([]string{"critique_upheld", "critique_rejected", "critique_note"}))
		Expect(results.GraphDeferredMetadataKey).To(Equal("graph_deferred"))
	})

	Describe("Apply", func() {
		crit := []results.CritiqueResult{
			critiqueOf(results.RelationshipAnalyzer, "a", "b", results.CritiqueVetoed, 0, 3, 0,
				"panel-1 s0 REJECT: unrelated", "panel-2 s0 REJECT: different scope", "panel-3 s0 REJECT: no overlap"),
			critiqueOf(results.RequiresAnalyzer, "a", "b", results.CritiqueUpheld, 3, 0, 0,
				"panel-1 s0 UPHOLD: yes", "panel-2 s0 UPHOLD: yes", "panel-3 s0 UPHOLD: yes"),
			critiqueOf(results.RequiresAnalyzer, "c", "d", results.CritiqueVetoed, 1, 2, 0,
				"panel-1 s0 UPHOLD: ok", "panel-2 s0 REJECT: no", "panel-3 s0 REJECT: no"),
		}

		DescribeTable("vetoes each kind's pairs independently",
			func(kind, src, tgt string, vetoed bool) {
				Expect(results.Apply(kind, crit).Vetoed(src, tgt)).To(Equal(vetoed))
			},
			Entry("a relationship veto drops the relationship verdict", results.RelationshipAnalyzer, "a", "b", true),
			Entry("a relationship veto leaves the pair's upheld requires verdict", results.RequiresAnalyzer, "a", "b", false),
			Entry("a requires veto drops the requires verdict", results.RequiresAnalyzer, "c", "d", true),
			Entry("a requires veto leaves the pair's relationship verdict", results.RelationshipAnalyzer, "c", "d", false),
			Entry("the reversed pair is a different pair", results.RelationshipAnalyzer, "b", "a", false),
		)

		It("keeps a pair the critique produced no result for, with no note", func() {
			v := results.Apply(results.RelationshipAnalyzer, crit)
			Expect(v.Vetoed("x", "y")).To(BeFalse())
			Expect(v.Note("x", "y")).To(BeEmpty())
			_, ok := v.Result("x", "y")
			Expect(ok).To(BeFalse())
		})

		It("returns the stored critique of a pair", func() {
			got, ok := results.Apply(results.RequiresAnalyzer, crit).Result("c", "d")
			Expect(ok).To(BeTrue())
			Expect(got).To(Equal(crit[2]))
		})

		DescribeTable("vetoes nothing and notes nothing without a critique",
			func(v results.Verdicts) {
				Expect(v.Vetoed("a", "b")).To(BeFalse())
				Expect(v.Note("a", "b")).To(BeEmpty())
				_, ok := v.Result("a", "b")
				Expect(ok).To(BeFalse())
			},
			Entry("no critique results", results.Apply(results.RelationshipAnalyzer, nil)),
			Entry("the zero value", results.Verdicts{}),
		)
	})

	DescribeTable("Note",
		func(c results.CritiqueResult, want string) {
			Expect(c.Note()).To(Equal(want))
			Expect(results.Apply(c.Kind, []results.CritiqueResult{c}).Note(c.SourceControlID, c.TargetControlID)).To(Equal(want))
		},
		Entry("an upheld critique has no note",
			critiqueOf(results.RelationshipAnalyzer, "a", "b", results.CritiqueUpheld, 3, 0, 0,
				"panel-1 s0 UPHOLD: x", "panel-2 s0 UPHOLD: y", "panel-3 s0 UPHOLD: z"), ""),
		Entry("a split critique names the rejections and the first reject reason",
			critiqueOf(results.RelationshipAnalyzer, "a", "b", results.CritiqueSplit, 2, 1, 0,
				"panel-1 s0 UPHOLD: fine", "panel-2 s0 REJECT: scope differs", "panel-3 s0 UPHOLD: fine"),
			"critique split: 1 of 3 rejected — panel-2 s0: scope differs"),
		Entry("a split critique counts failed votes in the panel size",
			critiqueOf(results.RequiresAnalyzer, "a", "b", results.CritiqueSplit, 1, 1, 1,
				"panel-1 s0 FAILED: the critique task failed", "panel-2 s0 REJECT: not a prerequisite", "panel-3 s0 UPHOLD: ok"),
			"critique split: 1 of 3 rejected — panel-2 s0: not a prerequisite"),
		Entry("a reason that only mentions REJECT is not a reject reason",
			critiqueOf(results.RelationshipAnalyzer, "a", "b", results.CritiqueSplit, 1, 1, 0,
				"panel-1 s0 UPHOLD: I would not REJECT: this", "panel-2 s0 REJECT: too broad"),
			"critique split: 1 of 2 rejected — panel-2 s0: too broad"),
		Entry("a split critique without a labelled reject reason names only the counts",
			critiqueOf(results.RelationshipAnalyzer, "a", "b", results.CritiqueSplit, 1, 1, 0),
			"critique split: 1 of 2 rejected"),
		Entry("a failed critique names the invalid votes",
			critiqueOf(results.RelationshipAnalyzer, "a", "b", results.CritiqueFailed, 0, 0, 3,
				"panel-1 s0 FAILED: x", "panel-2 s0 FAILED: y", "panel-3 s0 FAILED: z"),
			"critique failed: 3 of 3 votes invalid"),
		Entry("a failed critique over the error rate counts only the failed votes as invalid",
			critiqueOf(results.RelationshipAnalyzer, "a", "b", results.CritiqueFailed, 1, 0, 2),
			"critique failed: 2 of 3 votes invalid"),
		Entry("a vetoed critique has no note: the pair is dropped",
			critiqueOf(results.RelationshipAnalyzer, "a", "b", results.CritiqueVetoed, 0, 3, 0, "panel-1 s0 REJECT: x"), ""),
	)

	Describe("DecodeCritique", func() {
		It("decodes a stored critique result", func() {
			crit := []results.CritiqueResult{critiqueOf(results.RelationshipAnalyzer, "a", "b", results.CritiqueSplit, 2, 1, 0,
				"panel-1 s0 UPHOLD: ok", "panel-2 s0 REJECT: too broad", "panel-3 s0 UPHOLD: ok")}
			data, err := json.Marshal(crit)
			Expect(err).NotTo(HaveOccurred())
			Expect(results.DecodeCritique(data, "job-1")).To(Equal(crit))
		})

		DescribeTable("rejects a corrupt stored critique result, naming the job",
			func(data, want string) {
				got, err := results.DecodeCritique([]byte(data), "job-1")
				Expect(err).To(MatchError(results.ErrCorruptCritique))
				Expect(err).To(MatchError(ContainSubstring(want)))
				Expect(got).To(BeNil())
			},
			Entry("JSON that does not decode", `{not json`,
				"the stored critique result of job job-1 does not decode, so it is corrupt"),
			Entry("an object instead of a list", `{"x":1}`,
				"the stored critique result of job job-1 does not decode, so it is corrupt"),
			Entry("a negative vote count", `[{"kind":"relationship","source_control_id":"AC-1","target_control_id":"B-1","upheld":-1}]`,
				"the stored critique result of job job-1 gives pair AC-1 -> B-1 an impossible vote count (upheld -1, rejected 0, failed 0), so it is corrupt"),
			Entry("a vote count beyond int32", `[{"kind":"requires","source_control_id":"AC-1","target_control_id":"B-1","failed":2147483648}]`,
				"the stored critique result of job job-1 gives pair AC-1 -> B-1 an impossible vote count (upheld 0, rejected 0, failed 2147483648), so it is corrupt"),
		)
	})
})
