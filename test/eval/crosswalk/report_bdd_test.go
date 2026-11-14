package crosswalk_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/test/eval/crosswalk"
)

// fullReport scores a run that exercises every report section.
func fullReport() *crosswalk.Report {
	refs := []crosswalk.Reference{ref("s-1", "ab-2.3"), ref("s-2", "cd-1")}
	ms := []crosswalk.Mapping{mapping("s-1", "ab-2", equivalent, 0.8), mapping("s-2", "top.1", subsetOf, 1)}
	baseIn := scoreInput(refs, ms[:1], candidatesFor(ms[:1]))
	in := scoreInput(refs, ms, append(candidatesFor(ms), cand("s-2", "cd-1")))
	in.Refs.Unresolved = []crosswalk.RawReference{{SourceID: "s-1", Identifier: "ZZ-404", Citation: "c1"}}
	in.Refs.Ambiguous = []crosswalk.RawReference{{SourceID: "s-2", Identifier: "shared-9", Citation: "c2"}}
	t0 := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	in.Run.Stages = []crosswalk.Stage{{Name: "embedding", Status: "JOB_STATUS_COMPLETED", Started: t0, Completed: t0.Add(time.Minute)}}
	in.Run.Costs = []crosswalk.AnalyzerCost{{Analyzer: "relationship", Mode: "bypassed", Attestation: "attested", Tasks: 2, Dispatched: 6, Comparable: true, Models: []string{"m"}}}
	in.Baseline = mustScore(baseIn)
	return mustScore(in)
}

var _ = Describe("Report", func() {
	It("round-trips through report.json", func() {
		r := fullReport()
		raw, err := json.MarshalIndent(r, "", "  ")
		Expect(err).NotTo(HaveOccurred())
		path := filepath.Join(GinkgoT().TempDir(), "report.json")
		writeFile(path, string(raw))
		got, err := crosswalk.LoadReport(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(r))
	})

	It("serves as the next run's baseline after a round trip", func() {
		r := fullReport()
		raw, err := json.Marshal(r)
		Expect(err).NotTo(HaveOccurred())
		path := filepath.Join(GinkgoT().TempDir(), "report.json")
		writeFile(path, string(raw))
		baseline, err := crosswalk.LoadReport(path)
		Expect(err).NotTo(HaveOccurred())
		in := scoreInput([]crosswalk.Reference{ref("s-1", "ab-2.3"), ref("s-2", "cd-1")}, nil, nil)
		in.Baseline = baseline
		a := mustScore(in).Agreement
		Expect(a.OnlyBaseline).To(Equal([]crosswalk.Pair{{SourceID: "s-1", TargetID: "ab-2"}, {SourceID: "s-2", TargetID: "top.1"}}))
	})

	DescribeTable("LoadReport rejects input that is not a report, naming the file",
		func(content, want string) {
			path := filepath.Join(GinkgoT().TempDir(), "report.json")
			writeFile(path, content)
			_, err := crosswalk.LoadReport(path)
			Expect(err).To(MatchError(And(ContainSubstring(path), ContainSubstring(want))))
		},
		Entry("not JSON", "{", "decoding"),
		Entry("an unknown field", `{"provenance":{"profile":{"name":"p","digest":"d"}},"surprise":1}`, "surprise"),
		Entry("no profile digest", `{"provenance":{"profile":{"name":"p"}}}`, "profile digest"),
	)

	It("LoadReport names a missing file", func() {
		path := filepath.Join(GinkgoT().TempDir(), "absent.json")
		_, err := crosswalk.LoadReport(path)
		Expect(err).To(MatchError(ContainSubstring(path)))
	})

	Describe("Markdown", func() {
		It("shows the resolution rate first, then recall, reachability, cost, distributions and agreement", func() {
			md := fullReport().Markdown()
			order := []string{
				"Resolution: 2/4 (50.0%)",
				"| Found | 0/2 (0.0%) | 1/2 (50.0%) |",
				"| Reachable | 1/2 (50.0%) | 2/2 (100.0%) |",
				"## Cost",
				"## Distributions",
				"## Agreement",
			}
			last := -1
			for _, s := range order {
				i := strings.Index(md, s)
				Expect(i).To(BeNumerically(">", last), "%q missing or out of order in:\n%s", s, md)
				last = i
			}
		})

		It("names the inputs and lists unresolved and ambiguous references", func() {
			md := fullReport().Markdown()
			Expect(md).To(ContainSubstring("digest-1"))
			Expect(md).To(ContainSubstring("job-1"))
			Expect(md).To(ContainSubstring("cat-tgt"))
			Expect(md).To(ContainSubstring("| s-1 | ZZ-404 | c1 |  |"))
			Expect(md).To(ContainSubstring("| s-2 | shared-9 | c2 | cd-1, cd-9 |"))
		})

		It("escapes table cells so untrusted text cannot break the table", func() {
			in := scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, nil, nil)
			in.Refs.Unresolved = []crosswalk.RawReference{{SourceID: "s-1", Identifier: "A|B", Citation: "line1\nline2"}}
			md := mustScore(in).Markdown()
			Expect(md).To(ContainSubstring(`| s-1 | A\|B | line1 line2 |  |`))
		})

		It("says so when there is no baseline", func() {
			md := mustScore(scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, nil, nil)).Markdown()
			Expect(md).To(ContainSubstring("No baseline"))
		})
	})
})

var _ = Describe("Report precision section", func() {
	precisionMarkdown := func(labels map[crosswalk.Pair]crosswalk.Label) string {
		ms := []crosswalk.Mapping{mapping("s-1", "ab-2", equivalent, 0.8), mapping("s-2", "top.1", subsetOf, 0.8), mapping("s-1", "cd-9", subsetOf, 0.8)}
		in := scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, ms, candidatesFor(ms))
		in.Labels = labels
		md := mustScore(in).Markdown()
		start := strings.Index(md, "## Precision")
		Expect(start).To(BeNumerically(">=", 0), "no Precision section in:\n%s", md)
		return strings.SplitN(md[start:], "\n## Cost", 2)[0]
	}

	It("labels the reference rate a lower bound and says when there are no labels", func() {
		section := precisionMarkdown(nil)
		Expect(section).To(ContainSubstring("Reference agreement: 1/3 (33.3%) of produced rows (a lower bound"))
		Expect(section).To(ContainSubstring("No labels for this profile."))
		Expect(section).NotTo(ContainSubstring("Labelled precision"))
	})

	It("shows labelled precision, coverage, unlabelled and disputed rows", func() {
		section := precisionMarkdown(map[crosswalk.Pair]crosswalk.Label{
			{SourceID: "s-1", TargetID: "ab-2"}:  {Verdict: "related"},
			{SourceID: "s-2", TargetID: "top.1"}: {Verdict: "disputed"},
		})
		Expect(section).To(ContainSubstring("Labelled precision: 1/1 (100.0%)"))
		Expect(section).To(ContainSubstring("Label coverage: 1/3 (33.3%)"))
		Expect(section).To(ContainSubstring("Unlabelled rows (label these next):\n\n- s-1 -> cd-9\n"))
		Expect(section).To(ContainSubstring("Disputed rows (need a human decision):\n\n- s-2 -> top.1\n"))
	})

	It("names the labels digest in the provenance, and only when there are labels", func() {
		ms := []crosswalk.Mapping{mapping("s-1", "ab-2", equivalent, 0.8)}
		in := scoreInput([]crosswalk.Reference{ref("s-1", "ab-2")}, ms, candidatesFor(ms))
		Expect(mustScore(in).Markdown()).NotTo(ContainSubstring("Labels SHA-256"))
		in.LabelsDigest = "abc123"
		r := mustScore(in)
		Expect(r.Provenance.LabelsDigest).To(Equal("abc123"))
		Expect(r.Markdown()).To(ContainSubstring("- Labels SHA-256: abc123\n"))
	})
})
