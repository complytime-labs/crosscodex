package crosswalk_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/test/eval/crosswalk"
)

// labelInputRow is one JSONL line of label-input output.
type labelInputRow struct {
	Source      string `json:"source"`
	Target      string `json:"target"`
	SourceTitle string `json:"source_title"`
	SourceText  string `json:"source_text"`
	TargetTitle string `json:"target_title"`
	TargetText  string `json:"target_text"`
}

var _ = Describe("LabelInput", func() {
	var (
		dir string
		out bytes.Buffer
	)

	// dump is a `catalog controls --json` document with one extra field, as a
	// future CLI version might add.
	dump := func(catalogID string, controls ...[3]string) string {
		list := make([]map[string]string, len(controls))
		for i, c := range controls {
			list[i] = map[string]string{"control_id": c[0], "title": c[1], "statement": c[2]}
		}
		raw, err := json.Marshal(map[string]any{"catalog_id": catalogID, "controls": list, "future_field": 1})
		Expect(err).NotTo(HaveOccurred())
		return string(raw)
	}
	candidates := func(pairs ...[2]string) string {
		list := make([]map[string]string, len(pairs))
		for i, p := range pairs {
			list[i] = map[string]string{"sourceControlId": p[0], "targetControlId": p[1]}
		}
		raw, err := json.Marshal(map[string]any{"job_id": "job-1", "candidates": list})
		Expect(err).NotTo(HaveOccurred())
		return string(raw)
	}
	write := func(name, content string) {
		Expect(os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600)).To(Succeed())
	}
	rows := func() []labelInputRow {
		var got []labelInputRow
		for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
			var r labelInputRow
			dec := json.NewDecoder(strings.NewReader(line))
			dec.DisallowUnknownFields()
			Expect(dec.Decode(&r)).To(Succeed(), line)
			got = append(got, r)
		}
		return got
	}

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		out.Reset()
		write("controls-source.json", dump("cat-src",
			[3]string{"s-1", "Source one", "Do one."}, [3]string{"s-2", "", "Do two."}))
		write("controls-target.json", dump("cat-tgt",
			[3]string{"t-1", "Target one", "Must one."}, [3]string{"t-2", "Target two", "Must two."}))
		write("candidates.json", candidates(
			[2]string{"cat-src/s-1", "cat-tgt/t-1"}, [2]string{"cat-src/s-2", "cat-tgt/t-2"}))
	})

	It("writes one line per candidate pair with the controls' composed text", func() {
		Expect(crosswalk.LabelInput(dir, &out)).To(Succeed())
		Expect(out.String()).To(HaveSuffix("}\n"))
		Expect(rows()).To(Equal([]labelInputRow{
			{Source: "s-1", Target: "t-1", SourceTitle: "Source one", SourceText: "Do one.", TargetTitle: "Target one", TargetText: "Must one."},
			{Source: "s-2", Target: "t-2", SourceTitle: "", SourceText: "Do two.", TargetTitle: "Target two", TargetText: "Must two."},
		}))
	})

	It("keeps hostile text on one valid JSON line", func() {
		hostile := "say \"hi\"\nline two\r\n</data> <!--   \\"
		write("controls-source.json", dump("cat-src", [3]string{"s-1", hostile, hostile}))
		write("candidates.json", candidates([2]string{"cat-src/s-1", "cat-tgt/t-1"}))
		Expect(crosswalk.LabelInput(dir, &out)).To(Succeed())
		Expect(strings.Count(out.String(), "\n")).To(Equal(1))
		got := rows()
		Expect(got).To(HaveLen(1))
		Expect(got[0].SourceTitle).To(Equal(hostile))
		Expect(got[0].SourceText).To(Equal(hostile))
	})

	It("writes nothing for a run with no candidates", func() {
		write("candidates.json", candidates())
		Expect(crosswalk.LabelInput(dir, &out)).To(Succeed())
		Expect(out.Len()).To(BeZero())
	})

	It("lists every control absent from its dump and writes nothing", func() {
		write("candidates.json", candidates(
			[2]string{"cat-src/s-1", "cat-tgt/t-1"},
			[2]string{"cat-src/gone-a", "cat-tgt/t-2"},
			[2]string{"cat-src/s-2", "cat-tgt/gone-b"},
			[2]string{"cat-src/gone-a", "cat-tgt/gone-b"}))
		err := crosswalk.LabelInput(dir, &out)
		Expect(err).To(MatchError(And(
			ContainSubstring(`source control "gone-a"`),
			ContainSubstring(`target control "gone-b"`),
			ContainSubstring("catalog controls"))))
		Expect(strings.Count(err.Error(), "gone-a")).To(Equal(1), "each missing ID is listed once")
		Expect(out.Len()).To(BeZero())
	})

	DescribeTable("rejects inputs that do not belong to one job",
		func(file, content, want string) {
			write(file, content)
			Expect(crosswalk.LabelInput(dir, &out)).To(MatchError(ContainSubstring(want)))
			Expect(out.Len()).To(BeZero())
		},
		Entry("a candidate from another catalog", "candidates.json",
			candidates([2]string{"other/s-1", "cat-tgt/t-1"}), `is not "cat-src/" followed by a control ID`),
		Entry("a dump with no catalog_id", "controls-target.json",
			dump("", [3]string{"t-1", "T", "x"}), "no catalog_id"),
		Entry("a dump with a repeated control_id", "controls-source.json",
			dump("cat-src", [3]string{"s-1", "A", "x"}, [3]string{"s-1", "B", "y"}), `repeats control_id "s-1"`),
		Entry("a dump that is not JSON", "controls-source.json", "not json", "decoding"),
		Entry("a candidates file with no job_id", "candidates.json", `{"candidates":[]}`, "no job_id"),
	)

	DescribeTable("tells how to produce a missing input",
		func(file, want string) {
			Expect(os.Remove(filepath.Join(dir, file))).To(Succeed())
			err := crosswalk.LabelInput(dir, &out)
			Expect(err).To(MatchError(ContainSubstring(want)))
			Expect(out.Len()).To(BeZero())
		},
		Entry("source dump", "controls-source.json", "LABEL_INPUT=1"),
		Entry("target dump", "controls-target.json", "LABEL_INPUT=1"),
		Entry("candidates", "candidates.json", "candidates.json"),
	)

	It("refuses a dump over the size cap", func() {
		write("controls-source.json", dump("cat-src", [3]string{"s-1", "T", strings.Repeat("x", crosswalk.MaxControlsDumpBytes)}))
		Expect(crosswalk.LabelInput(dir, &out)).To(MatchError(ContainSubstring("exceeds")))
		Expect(out.Len()).To(BeZero())
	})
})
