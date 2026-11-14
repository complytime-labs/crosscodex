package crosswalk_test

import (
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/test/eval/crosswalk"
)

// entry renders one labels entry; fields override the defaults, and a field
// set to "-" is left out.
func entry(overrides map[string]string) string {
	fields := []string{"source", "target", "verdict", "judged_by", "rationale", "date"}
	vals := map[string]string{
		"source": "PO.1.1", "target": "sa-8", "verdict": "related",
		"judged_by": "[opus, sonnet]", "rationale": "Both define security requirements.", "date": "2026-11-12",
	}
	for k, v := range overrides {
		vals[k] = v
	}
	var b strings.Builder
	first := true
	for _, f := range fields {
		if vals[f] == "-" {
			continue
		}
		prefix := "    "
		if first {
			prefix, first = "  - ", false
		}
		b.WriteString(prefix + f + ": " + vals[f] + "\n")
	}
	return b.String()
}

func labelsFile(body string) string {
	path := filepath.Join(GinkgoT().TempDir(), "labels.yaml")
	Expect(os.WriteFile(path, []byte(body), 0o600)).To(Succeed())
	return path
}

func humanOver(o map[string]string) map[string]string {
	o["judged_by"] = "[human]"
	return o
}

var _ = Describe("LoadLabels", func() {
	It("loads a valid file keyed by pair, with each entry's line", func() {
		got, _, err := crosswalk.LoadLabels(labelsFile("# note\nversion: 1\npairs:\n" +
			entry(nil) + entry(map[string]string{"target": "sa-9", "verdict": "unrelated", "judged_by": "[sonnet]"})))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(2))
		Expect(got[crosswalk.Pair{SourceID: "PO.1.1", TargetID: "sa-8"}]).To(Equal(crosswalk.Label{
			Source: "PO.1.1", Target: "sa-8", Verdict: "related", JudgedBy: []string{"opus", "sonnet"},
			Rationale: "Both define security requirements.", Date: "2026-11-12", Line: 4,
		}))
		Expect(got[crosswalk.Pair{SourceID: "PO.1.1", TargetID: "sa-9"}].Line).To(Equal(10))
	})

	It("accepts a file with no pairs", func() {
		got, _, err := crosswalk.LoadLabels(labelsFile("version: 1\npairs: []\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())
	})

	It("accepts a BOM and CRLF line endings", func() {
		body := "\xef\xbb\xbfversion: 1\npairs:\n" + entry(nil)
		got, _, err := crosswalk.LoadLabels(labelsFile(strings.ReplaceAll(body, "\n", "\r\n")))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(1))
		Expect(got[crosswalk.Pair{SourceID: "PO.1.1", TargetID: "sa-8"}].Rationale).To(Equal("Both define security requirements."))
	})

	DescribeTable("rejects an invalid file, naming the file, the line and the rule",
		func(body, line, rule string) {
			path := labelsFile(body)
			_, _, err := crosswalk.LoadLabels(path)
			Expect(err).To(MatchError(And(ContainSubstring(path), ContainSubstring("line "+line), ContainSubstring("rule "+rule))))
		},
		Entry("no version", "pairs: []\n", "1", "version"),
		Entry("another version", "version: 2\npairs: []\n", "1", "version"),
		Entry("an unknown top-level field", "version: 1\nextra: x\n", "2", "unknown-field"),
		Entry("pairs that is not a list", "version: 1\npairs: x\n", "2", "pairs"),
		Entry("an unknown entry field", "version: 1\npairs:\n"+entry(nil)+"    note: x\n", "9", "unknown-field"),
		Entry("a repeated entry field", "version: 1\npairs:\n"+entry(nil)+"    verdict: related\n", "9", "duplicate-field"),
		Entry("an entry that is not a mapping", "version: 1\npairs:\n  - x\n", "3", "a pairs entry"),
		Entry("a missing source", "version: 1\npairs:\n"+entry(map[string]string{"source": "-"}), "3", "source"),
		Entry("a blank target", "version: 1\npairs:\n"+entry(map[string]string{"target": `" "`}), "4", "target"),
		Entry("a control character in a source", "version: 1\npairs:\n"+entry(map[string]string{"source": `"a\u001bb"`}), "3", "source"),
		Entry("an unknown verdict", "version: 1\npairs:\n"+entry(map[string]string{"verdict": "maybe"}), "5", "verdict"),
		Entry("a missing verdict", "version: 1\npairs:\n"+entry(map[string]string{"verdict": "-"}), "3", "verdict"),
		Entry("judged_by that is a scalar", "version: 1\npairs:\n"+entry(map[string]string{"judged_by": "opus"}), "6", "judged_by"),
		Entry("an empty judged_by", "version: 1\npairs:\n"+entry(map[string]string{"judged_by": "[]"}), "6", "judged_by"),
		Entry("an unknown judge", "version: 1\npairs:\n"+entry(map[string]string{"judged_by": "[gpt]"}), "6", "judged_by"),
		Entry("a judge listed twice", "version: 1\npairs:\n"+entry(map[string]string{"judged_by": "[opus, opus]"}), "6", "judged_by"),
		Entry("a missing judged_by", "version: 1\npairs:\n"+entry(map[string]string{"judged_by": "-"}), "3", "judged_by"),
		Entry("a missing rationale", "version: 1\npairs:\n"+entry(map[string]string{"rationale": "-"}), "3", "rationale"),
		Entry("a rationale over the limit", "version: 1\npairs:\n"+entry(map[string]string{"rationale": strings.Repeat("x", 5000)}), "7", "rationale"),
		Entry("a missing date", "version: 1\npairs:\n"+entry(map[string]string{"date": "-"}), "3", "date"),
		Entry("a date that is not a date", "version: 1\npairs:\n"+entry(map[string]string{"date": "soon"}), "8", "date"),
		Entry("a date that does not exist", "version: 1\npairs:\n"+entry(map[string]string{"date": "2026-02-30"}), "8", "date"),
		Entry("a date without zero padding", "version: 1\npairs:\n"+entry(map[string]string{"date": "2026-1-2"}), "8", "date"),
		Entry("an alias", "version: 1\npairs:\n"+entry(map[string]string{"rationale": "&r x"})+"  - *r\n", "9", "alias"),
	)

	DescribeTable("rejects a file that is not one YAML mapping",
		func(body, fragment string) {
			_, _, err := crosswalk.LoadLabels(labelsFile(body))
			Expect(err).To(MatchError(ContainSubstring(fragment)))
		},
		Entry("empty", "", "is empty"),
		Entry("only a comment", "# nothing\n", "is empty"),
		Entry("a list", "- a\n", "must be a mapping"),
		Entry("two documents", "version: 1\n---\nversion: 1\n", "single YAML document"),
		Entry("invalid YAML", "version: [\n", "decoding labels"),
		Entry("binary garbage", "\xff\x00\x01", "labels"),
	)

	It("fails on a missing file, naming it", func() {
		path := filepath.Join(GinkgoT().TempDir(), "absent.yaml")
		_, _, err := crosswalk.LoadLabels(path)
		Expect(err).To(MatchError(ContainSubstring(path)))
	})

	DescribeTable("resolves two entries for one pair",
		func(first, second map[string]string, winner string) {
			got, _, err := crosswalk.LoadLabels(labelsFile("version: 1\npairs:\n" + entry(first) + entry(second)))
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(HaveLen(1))
			Expect(got[crosswalk.Pair{SourceID: "PO.1.1", TargetID: "sa-8"}].Verdict).To(Equal(winner))
		},
		Entry("a later human entry overrides a model entry", map[string]string{"verdict": "related"}, humanOver(map[string]string{"verdict": "unrelated"}), "unrelated"),
		Entry("an earlier human entry survives a later model entry", humanOver(map[string]string{"verdict": "unrelated"}), map[string]string{"verdict": "related"}, "unrelated"),
		Entry("a mixed model and human entry counts as human", map[string]string{"verdict": "related"}, map[string]string{"verdict": "disputed", "judged_by": "[opus, human]"}, "disputed"),
	)

	DescribeTable("rejects a repeated model entry even when a human entry is also present",
		func(entries []map[string]string, lines string) {
			body := "version: 1\npairs:\n"
			for _, e := range entries {
				body += entry(e)
			}
			_, _, err := crosswalk.LoadLabels(labelsFile(body))
			Expect(err).To(MatchError(And(ContainSubstring("rule duplicate-pair"), ContainSubstring("lines "+lines))))
		},
		Entry("model, human, model", []map[string]string{nil, humanOver(map[string]string{}), {"judged_by": "[sonnet]"}}, "3 and 15"),
		Entry("human, model, model", []map[string]string{humanOver(map[string]string{}), nil, {"judged_by": "[sonnet]"}}, "9 and 15"),
		Entry("model, model, human", []map[string]string{nil, {"judged_by": "[sonnet]"}, humanOver(map[string]string{})}, "3 and 9"),
	)

	DescribeTable("rejects a source or target over the length limit",
		func(field string) {
			path := labelsFile("version: 1\npairs:\n" + entry(map[string]string{field: strings.Repeat("a", 129)}))
			_, _, err := crosswalk.LoadLabels(path)
			Expect(err).To(MatchError(And(ContainSubstring(path), ContainSubstring("rule "+field), ContainSubstring("limit is 128"))))
		},
		Entry("source", "source"),
		Entry("target", "target"),
	)

	DescribeTable("returns the SHA-256 of the exact bytes it parsed",
		func(body, want string) {
			got, digest, err := crosswalk.LoadLabels(labelsFile(body))
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(BeEmpty())
			Expect(digest).To(Equal(want))
		},
		Entry("a plain file", "version: 1\npairs: []\n", "ce976dd9c17bebb1fb4dbe519274e297d61538377479e91bf9e1764ae77f8d76"),
		Entry("a file with a BOM, hashed with the BOM", "\xef\xbb\xbfversion: 1\npairs: []\n",
			"e2352548fdf6c7968932f811494b6fed510173bad89adc09ed634d170875ca22"),
	)

	DescribeTable("rejects two entries for one pair that no human overrides",
		func(first, second map[string]string, fragment string) {
			_, _, err := crosswalk.LoadLabels(labelsFile("version: 1\npairs:\n" + entry(first) + entry(second)))
			Expect(err).To(MatchError(And(ContainSubstring("rule duplicate-pair"), ContainSubstring("lines 3 and 9"), ContainSubstring(fragment))))
		},
		Entry("two model entries", nil, map[string]string{"judged_by": "[sonnet]"}, "add a human entry"),
		Entry("two human entries", humanOver(map[string]string{}), humanOver(map[string]string{"verdict": "unrelated"}), "judged by a human"),
	)
})

// Seeded label files are discovered at tree-construction time (specs run in
// the package directory), so a new file under labels/ is covered unasked.
var _ = Describe("Seeded label files", func() {
	files, globErr := filepath.Glob(filepath.Join("labels", "*.yaml"))

	It("finds at least one label file", func() {
		Expect(globErr).NotTo(HaveOccurred())
		Expect(files).NotTo(BeEmpty(), "no *.yaml under test/eval/crosswalk/labels/")
	})

	for _, file := range files {
		It("loads "+file+" with at least one label", func() {
			got, digest, err := crosswalk.LoadLabels(file)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).NotTo(BeEmpty())
			Expect(digest).To(HaveLen(64))
		})
	}
})
