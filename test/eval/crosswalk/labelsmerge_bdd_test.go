package crosswalk_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/test/eval/crosswalk"
)

const mergeDate = "2026-11-24"

// judgeLine renders one judge JSONL line.
func judgeLine(source, target, verdict, rationale string) string {
	return fmt.Sprintf(`{"source":%q,"target":%q,"verdict":%q,"rationale":%q}`+"\n", source, target, verdict, rationale)
}

// pairLine renders one label-input JSONL line, with the text fields a real
// file carries and the merge ignores.
func pairLine(source, target string) string {
	return fmt.Sprintf(`{"source":%q,"target":%q,"source_title":"t","source_text":"x","target_title":"u","target_text":"y"}`+"\n", source, target)
}

var _ = Describe("MergeLabelFiles", func() {
	var dir string

	BeforeEach(func() { dir = GinkgoT().TempDir() })

	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		Expect(os.WriteFile(path, []byte(content), 0o600)).To(Succeed())
		return path
	}
	// request is a merge of the pairs file and the two judge files, with no
	// existing labels.
	request := func(pairs, opus, sonnet string) crosswalk.MergeRequest {
		return crosswalk.MergeRequest{
			Pairs:  write("pairs.jsonl", pairs),
			Judges: map[string]string{"opus": write("opus.jsonl", opus), "sonnet": write("sonnet.jsonl", sonnet)},
			Out:    filepath.Join(dir, "out.yaml"),
			Date:   mergeDate,
		}
	}
	loaded := func(path string) map[crosswalk.Pair]crosswalk.Label {
		labels, _, err := crosswalk.LoadLabels(path)
		Expect(err).NotTo(HaveOccurred())
		return labels
	}
	pair := func(s, t string) crosswalk.Pair { return crosswalk.Pair{SourceID: s, TargetID: t} }

	It("writes one entry for a pair both judges agree on, with the first judge's rationale", func() {
		req := request(pairLine("a", "x"),
			judgeLine("a", "x", "related", "opus says so"), judgeLine("a", "x", "related", "sonnet says so"))
		disputed, err := crosswalk.MergeLabelFiles(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(disputed).To(BeEmpty())
		l := loaded(req.Out)[pair("a", "x")]
		Expect(l.Verdict).To(Equal("related"))
		Expect(l.JudgedBy).To(Equal([]string{"opus", "sonnet"}))
		Expect(l.Rationale).To(Equal("opus says so"))
		Expect(l.Date).To(Equal(mergeDate))
	})

	It("marks a disagreement disputed, keeps both rationales and reports the pair", func() {
		req := request(pairLine("a", "x"),
			judgeLine("a", "x", "related", "same topic"), judgeLine("a", "x", "unrelated", "different scope"))
		disputed, err := crosswalk.MergeLabelFiles(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(disputed).To(Equal([]crosswalk.Pair{pair("a", "x")}))
		l := loaded(req.Out)[pair("a", "x")]
		Expect(l.Verdict).To(Equal("disputed"))
		Expect(l.JudgedBy).To(Equal([]string{"opus", "sonnet"}))
		Expect(l.Rationale).To(And(ContainSubstring("opus: same topic"), ContainSubstring("sonnet: different scope")))
	})

	It("keeps a human entry and emits no model entry for its pair", func() {
		existing := write("labels.yaml", "version: 1\npairs:\n"+entry(map[string]string{
			"source": "a", "target": "x", "verdict": "unrelated", "judged_by": "[human]", "rationale": "Human call."}))
		req := request(pairLine("a", "x")+pairLine("b", "y"),
			judgeLine("a", "x", "related", "o1")+judgeLine("b", "y", "related", "o2"),
			judgeLine("a", "x", "unrelated", "s1")+judgeLine("b", "y", "related", "s2"))
		req.Existing = existing
		disputed, err := crosswalk.MergeLabelFiles(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(disputed).To(BeEmpty(), "a human-judged pair is settled, not disputed")
		got := loaded(req.Out)
		Expect(got).To(HaveLen(2))
		Expect(got[pair("a", "x")].JudgedBy).To(Equal([]string{"human"}))
		Expect(got[pair("a", "x")].Verdict).To(Equal("unrelated"))
		Expect(got[pair("a", "x")].Rationale).To(Equal("Human call."))
		Expect(got[pair("a", "x")].Date).To(Equal("2026-11-12"))
		Expect(got[pair("b", "y")].JudgedBy).To(Equal([]string{"opus", "sonnet"}))
	})

	It("keeps only the human entry when the existing file holds a human and a model entry for one pair", func() {
		existing := write("labels.yaml", "version: 1\npairs:\n"+
			entry(map[string]string{"source": "a", "target": "x", "verdict": "unrelated", "judged_by": "[opus]", "rationale": "model call"})+
			entry(map[string]string{"source": "a", "target": "x", "verdict": "related", "judged_by": "[human]", "rationale": "Human call."}))
		req := request(pairLine("a", "x"), judgeLine("a", "x", "related", "o1"), judgeLine("a", "x", "unrelated", "s1"))
		req.Existing = existing
		disputed, err := crosswalk.MergeLabelFiles(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(disputed).To(BeEmpty(), "a human-judged pair is settled, not disputed")
		got := loaded(req.Out)
		Expect(got).To(HaveLen(1))
		Expect(got[pair("a", "x")].JudgedBy).To(Equal([]string{"human"}))
		Expect(got[pair("a", "x")].Verdict).To(Equal("related"))
		Expect(got[pair("a", "x")].Rationale).To(Equal("Human call."))
	})

	It("replaces an existing model entry for a required pair and keeps entries for other pairs", func() {
		existing := write("labels.yaml", "version: 1\npairs:\n"+
			entry(map[string]string{"source": "a", "target": "x", "verdict": "unrelated", "rationale": "old"})+
			entry(map[string]string{"source": "keep", "target": "me", "rationale": "untouched"}))
		req := request(pairLine("a", "x"), judgeLine("a", "x", "related", "new"), judgeLine("a", "x", "related", "new2"))
		req.Existing = existing
		_, err := crosswalk.MergeLabelFiles(req)
		Expect(err).NotTo(HaveOccurred())
		got := loaded(req.Out)
		Expect(got[pair("a", "x")].Rationale).To(Equal("new"))
		Expect(got[pair("keep", "me")].Rationale).To(Equal("untouched"))
		Expect(got[pair("keep", "me")].Date).To(Equal("2026-11-12"))
	})

	It("is byte-identical when merged again from its own output", func() {
		req := request(pairLine("b", "y")+pairLine("a", "x"),
			judgeLine("a", "x", "related", "o1")+judgeLine("b", "y", "related", "o2"),
			judgeLine("b", "y", "unrelated", "s2")+judgeLine("a", "x", "related", "s1"))
		_, err := crosswalk.MergeLabelFiles(req)
		Expect(err).NotTo(HaveOccurred())
		first, err := os.ReadFile(req.Out)
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Index(string(first), "source: a")).To(BeNumerically("<", strings.Index(string(first), "source: b")), "sorted by source")
		req.Existing = req.Out
		_, err = crosswalk.MergeLabelFiles(req)
		Expect(err).NotTo(HaveOccurred())
		second, err := os.ReadFile(req.Out)
		Expect(err).NotTo(HaveOccurred())
		Expect(second).To(Equal(first))
	})

	Describe("refusals leave the output untouched", func() {
		const prior = "prior output\n"

		DescribeTable("error",
			func(pairs, opus, sonnet string, wantErr ...string) {
				req := request(pairs, opus, sonnet)
				Expect(os.WriteFile(req.Out, []byte(prior), 0o600)).To(Succeed())
				disputed, err := crosswalk.MergeLabelFiles(req)
				Expect(err).To(HaveOccurred())
				for _, w := range wantErr {
					Expect(err.Error()).To(ContainSubstring(w))
				}
				Expect(disputed).To(BeEmpty())
				got, rerr := os.ReadFile(req.Out)
				Expect(rerr).NotTo(HaveOccurred())
				Expect(string(got)).To(Equal(prior))
				entries, derr := os.ReadDir(dir)
				Expect(derr).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(4), "no temp file is left behind")
			},
			Entry("a judge missing pairs lists every missing pair of every judge",
				pairLine("a", "x")+pairLine("b", "y")+pairLine("c", "z"),
				judgeLine("a", "x", "related", "r"),
				judgeLine("a", "x", "related", "r")+judgeLine("c", "z", "related", "r"),
				"opus", "b -> y", "c -> z", "sonnet", "missing"),
			Entry("a judged pair outside the required set",
				pairLine("a", "x"),
				judgeLine("a", "x", "related", "r")+judgeLine("q", "w", "related", "r"),
				judgeLine("a", "x", "related", "r"),
				"opus", "q -> w", "not in --pairs"),
			Entry("a pair judged twice in one file",
				pairLine("a", "x"),
				judgeLine("a", "x", "related", "r")+judgeLine("a", "x", "unrelated", "r"),
				judgeLine("a", "x", "related", "r"),
				"opus.jsonl", "line 2", "line 1"),
			Entry("an unknown JSON field",
				pairLine("a", "x"),
				`{"source":"a","target":"x","verdict":"related","rationale":"r","extra":1}`+"\n",
				judgeLine("a", "x", "related", "r"),
				"opus.jsonl", "line 1", "extra"),
			Entry("a key repeated within one line",
				pairLine("a", "x"),
				`{"source":"a","target":"x","verdict":"unrelated","verdict":"related","rationale":"r"}`+"\n",
				judgeLine("a", "x", "related", "r"),
				"opus.jsonl", "line 1", `"verdict"`, "more than once"),
			Entry("a key that differs only in case",
				pairLine("a", "x"),
				`{"source":"a","target":"x","VERDICT":"related","rationale":"r"}`+"\n",
				judgeLine("a", "x", "related", "r"),
				"opus.jsonl", "line 1", `"VERDICT"`, "exactly"),
			Entry("a key repeated through a unicode escape",
				pairLine("a", "x"),
				`{"source":"a","target":"x","verdict":"unrelated","\u0076erdict":"related","rationale":"r"}`+"\n",
				judgeLine("a", "x", "related", "r"),
				"opus.jsonl", "line 1", "more than once"),
			Entry("a key that differs only in case through a unicode escape",
				pairLine("a", "x"),
				`{"source":"a","target":"x","\u0056ERDICT":"related","rationale":"r"}`+"\n",
				judgeLine("a", "x", "related", "r"),
				"opus.jsonl", "line 1", "exactly"),
			Entry("a nested object as a value",
				pairLine("a", "x"),
				`{"source":"a","target":"x","verdict":"related","rationale":{"text":"r"}}`+"\n",
				judgeLine("a", "x", "related", "r"),
				"opus.jsonl", "line 1", `"rationale"`, "string"),
			Entry("a judge verdict of disputed",
				pairLine("a", "x"),
				judgeLine("a", "x", "disputed", "r"),
				judgeLine("a", "x", "related", "r"),
				"line 1", "related or unrelated"),
			Entry("a verdict of any other value",
				pairLine("a", "x"),
				judgeLine("a", "x", "maybe", "r"),
				judgeLine("a", "x", "related", "r"),
				"line 1", `"maybe"`),
			Entry("a blank rationale",
				pairLine("a", "x"),
				judgeLine("a", "x", "related", "  "),
				judgeLine("a", "x", "related", "r"),
				"line 1", "rationale"),
			Entry("an over-long rationale",
				pairLine("a", "x"),
				judgeLine("a", "x", "related", strings.Repeat("r", 5000)),
				judgeLine("a", "x", "related", "r"),
				"line 1", "rationale", "limit"),
			Entry("an over-long ID",
				pairLine("a", "x"),
				judgeLine(strings.Repeat("a", 200), "x", "related", "r"),
				judgeLine("a", "x", "related", "r"),
				"line 1", "source", "limit"),
			Entry("an over-long line",
				pairLine("a", "x"),
				judgeLine("a", "x", "related", "r")+strings.Repeat(" ", 40<<10)+"\n",
				judgeLine("a", "x", "related", "r"),
				"opus.jsonl", "line 2", "longer than"),
			Entry("an over-size file",
				pairLine("a", "x"),
				judgeLine("a", "x", "related", "r")+strings.Repeat("\n", 9<<20),
				judgeLine("a", "x", "related", "r"),
				"opus.jsonl", "exceeds"),
			Entry("trailing data after the object",
				pairLine("a", "x"),
				judgeLine("a", "x", "related", "r")[:len(judgeLine("a", "x", "related", "r"))-1]+" {}\n",
				judgeLine("a", "x", "related", "r"),
				"line 1", "single JSON object"),
			Entry("a line that is not JSON",
				pairLine("a", "x"),
				"not json\n",
				judgeLine("a", "x", "related", "r"),
				"opus.jsonl", "line 1"),
			Entry("an empty judge file",
				pairLine("a", "x"), "", judgeLine("a", "x", "related", "r"),
				"opus", "a -> x"),
			Entry("a repeated pair in the pairs file",
				pairLine("a", "x")+pairLine("a", "x"),
				judgeLine("a", "x", "related", "r"), judgeLine("a", "x", "related", "r"),
				"pairs.jsonl", "line 2"),
			Entry("an empty pairs file",
				"", judgeLine("a", "x", "related", "r"), judgeLine("a", "x", "related", "r"),
				"pairs.jsonl", "no pairs"),
		)

		It("refuses an existing labels file that fails the strict loader", func() {
			req := request(pairLine("a", "x"), judgeLine("a", "x", "related", "r"), judgeLine("a", "x", "related", "r"))
			req.Existing = write("bad.yaml", "version: 2\n")
			Expect(os.WriteFile(req.Out, []byte(prior), 0o600)).To(Succeed())
			_, err := crosswalk.MergeLabelFiles(req)
			Expect(err).To(MatchError(ContainSubstring("version must be 1")))
			got, rerr := os.ReadFile(req.Out)
			Expect(rerr).NotTo(HaveOccurred())
			Expect(string(got)).To(Equal(prior))
		})

		It("refuses a nonexistent existing labels file", func() {
			req := request(pairLine("a", "x"), judgeLine("a", "x", "related", "r"), judgeLine("a", "x", "related", "r"))
			req.Existing = filepath.Join(dir, "absent.yaml")
			_, err := crosswalk.MergeLabelFiles(req)
			Expect(err).To(MatchError(ContainSubstring("absent.yaml")))
			Expect(req.Out).NotTo(BeAnExistingFile())
		})

		DescribeTable("a judge name that is not a model judge",
			func(name string) {
				req := request(pairLine("a", "x"), judgeLine("a", "x", "related", "r"), judgeLine("a", "x", "related", "r"))
				req.Judges[name] = req.Judges["opus"]
				_, err := crosswalk.MergeLabelFiles(req)
				Expect(err).To(MatchError(ContainSubstring(name)))
				Expect(err).To(MatchError(ContainSubstring("opus, sonnet")))
				Expect(req.Out).NotTo(BeAnExistingFile())
			},
			Entry("human", "human"),
			Entry("an unknown model", "gpt"),
		)

		It("refuses a merge with no judge", func() {
			req := request(pairLine("a", "x"), "", "")
			req.Judges = nil
			_, err := crosswalk.MergeLabelFiles(req)
			Expect(err).To(MatchError(ContainSubstring("at least one judge")))
		})

		It("refuses a date that is not a calendar date", func() {
			req := request(pairLine("a", "x"), judgeLine("a", "x", "related", "r"), judgeLine("a", "x", "related", "r"))
			req.Date = "2026-02-30"
			_, err := crosswalk.MergeLabelFiles(req)
			Expect(err).To(MatchError(ContainSubstring("2026-02-30")))
			Expect(req.Out).NotTo(BeAnExistingFile())
		})
	})

	It("keeps a combined rationale within the labels limit", func() {
		long := strings.Repeat("é", 1000)
		req := request(pairLine("a", "x"), judgeLine("a", "x", "related", long), judgeLine("a", "x", "unrelated", long))
		_, err := crosswalk.MergeLabelFiles(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(loaded(req.Out)[pair("a", "x")].Verdict).To(Equal("disputed"))
	})
})

var _ = Describe("MergeLabels", func() {
	pairs := []crosswalk.Pair{{SourceID: "a", TargetID: "x"}}

	It("refuses a human entry only when it would be altered, never altering it", func() {
		human := crosswalk.Label{Source: "a", Target: "x", Verdict: "unrelated", JudgedBy: []string{"human"}, Rationale: "mine", Date: "2026-01-01"}
		judges := map[string][]crosswalk.JudgeEntry{
			"opus":   {{Source: "a", Target: "x", Verdict: "related", Rationale: "o"}},
			"sonnet": {{Source: "a", Target: "x", Verdict: "related", Rationale: "s"}},
		}
		merged, disputed, err := crosswalk.MergeLabels([]crosswalk.Label{human}, judges, pairs, mergeDate)
		Expect(err).NotTo(HaveOccurred())
		Expect(disputed).To(BeEmpty())
		Expect(merged).To(Equal([]crosswalk.Label{human}))
	})

	It("rejects two existing entries for one pair", func() {
		l := crosswalk.Label{Source: "a", Target: "x", Verdict: "related", JudgedBy: []string{"opus"}, Rationale: "r", Date: "2026-01-01"}
		_, _, err := crosswalk.MergeLabels([]crosswalk.Label{l, l}, map[string][]crosswalk.JudgeEntry{"opus": nil}, nil, mergeDate)
		Expect(err).To(MatchError(ContainSubstring("a -> x")))
	})
})

var _ = Describe("CheckRoundTrip", func() {
	merged := []crosswalk.Label{
		{Source: "a", Target: "x", Verdict: "related", JudgedBy: []string{"opus", "sonnet"}, Rationale: "r", Date: mergeDate},
		{Source: "b", Target: "y", Verdict: "unrelated", JudgedBy: []string{"human"}, Rationale: "h", Date: mergeDate},
	}
	// readBack is what the loader returns for merged, with Line set as it does.
	readBack := func() map[crosswalk.Pair]crosswalk.Label {
		back := map[crosswalk.Pair]crosswalk.Label{}
		for i, l := range merged {
			l.Line = i + 3
			back[crosswalk.Pair{SourceID: l.Source, TargetID: l.Target}] = l
		}
		return back
	}

	It("accepts labels that read back equal, ignoring line numbers", func() {
		Expect(crosswalk.CheckRoundTrip(merged, readBack())).To(Succeed())
	})

	DescribeTable("refuses a read-back that differs",
		func(mutate func(back map[crosswalk.Pair]crosswalk.Label), wantErr string) {
			back := readBack()
			mutate(back)
			Expect(crosswalk.CheckRoundTrip(merged, back)).To(MatchError(And(
				ContainSubstring(wantErr), ContainSubstring("not written"))))
		},
		Entry("a changed verdict", func(b map[crosswalk.Pair]crosswalk.Label) {
			l := b[crosswalk.Pair{SourceID: "a", TargetID: "x"}]
			l.Verdict = "unrelated"
			b[crosswalk.Pair{SourceID: "a", TargetID: "x"}] = l
		}, "a -> x"),
		Entry("a changed rationale", func(b map[crosswalk.Pair]crosswalk.Label) {
			l := b[crosswalk.Pair{SourceID: "b", TargetID: "y"}]
			l.Rationale = "h "
			b[crosswalk.Pair{SourceID: "b", TargetID: "y"}] = l
		}, "b -> y"),
		Entry("changed judges", func(b map[crosswalk.Pair]crosswalk.Label) {
			l := b[crosswalk.Pair{SourceID: "a", TargetID: "x"}]
			l.JudgedBy = []string{"opus"}
			b[crosswalk.Pair{SourceID: "a", TargetID: "x"}] = l
		}, "a -> x"),
		Entry("a changed date", func(b map[crosswalk.Pair]crosswalk.Label) {
			l := b[crosswalk.Pair{SourceID: "a", TargetID: "x"}]
			l.Date = "2000-01-01"
			b[crosswalk.Pair{SourceID: "a", TargetID: "x"}] = l
		}, "a -> x"),
		Entry("a missing pair", func(b map[crosswalk.Pair]crosswalk.Label) {
			delete(b, crosswalk.Pair{SourceID: "a", TargetID: "x"})
		}, "2 entries but 1"),
		Entry("a different pair in place of one", func(b map[crosswalk.Pair]crosswalk.Label) {
			delete(b, crosswalk.Pair{SourceID: "a", TargetID: "x"})
			b[crosswalk.Pair{SourceID: "c", TargetID: "z"}] = crosswalk.Label{Source: "c", Target: "z"}
		}, "a -> x"),
	)
})
