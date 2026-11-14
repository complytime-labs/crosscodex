package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	testdata  = "../../testdata"
	synthRoot = "../../../../.."
)

var _ = Describe("run", func() {
	var stdout, stderr bytes.Buffer

	BeforeEach(func() {
		stdout.Reset()
		stderr.Reset()
	})

	DescribeTable("exit codes",
		func(args []string, want int, stderrHas string) {
			Expect(run(args, &stdout, &stderr)).To(Equal(want))
			Expect(stderr.String()).To(ContainSubstring(stderrHas))
		},
		Entry("no subcommand is a usage error", nil, 2, "Usage: crosswalk-eval"),
		Entry("unknown subcommand is a usage error", []string{"bogus"}, 2, `unknown subcommand "bogus"`),
		Entry("missing required flag is a usage error", []string{"discover", "--source", "a.json"}, 2, "missing required flag --target"),
		Entry("unknown flag is a usage error", []string{"discover", "--nope"}, 2, "flag provided but not defined"),
		Entry("stray argument is a usage error", []string{"discover", "--source", "a", "--target", "b", "extra"}, 2, `unexpected argument "extra"`),
		Entry("missing catalog is an input error", []string{"discover", "--source", "absent.json", "--target", "absent.json"}, 1, "task fetch:oscal-docs"),
	)

	It("lists discover rows as JSON", func() {
		cat := filepath.Join(testdata, "synthetic_catalog.json")
		Expect(run([]string{"discover", "--source", filepath.Join(testdata, "source_a.json"), "--target", cat, "--json"}, &stdout, &stderr)).To(Equal(0))
		var rows []map[string]any
		Expect(json.Unmarshal(stdout.Bytes(), &rows)).To(Succeed())
		Expect(rows).NotTo(BeEmpty())
	})

	It("refuses a profile whose paths escape the root", func() {
		dir := GinkgoT().TempDir()
		profile := filepath.Join(dir, "p.yaml")
		Expect(os.WriteFile(profile, []byte(`name: escape
source: ../../etc/passwd
target: catalogs/t.json
controls: [a]
references:
  csv: refs.csv
`), 0o600)).To(Succeed())
		out := filepath.Join(dir, "sub.json")
		Expect(run([]string{"subset", "--root", dir, "--profile", profile, "--out", out}, &stdout, &stderr)).To(Equal(1))
		Expect(stderr.String()).To(ContainSubstring("must be relative to the repository root"))
		Expect(out).NotTo(BeAnExistingFile())
		Expect(stdout.String()).To(BeEmpty())
	})

	It("writes a subset catalog for a profile", func() {
		out := filepath.Join(GinkgoT().TempDir(), "sub.json")
		Expect(run([]string{"subset", "--root", synthRoot, "--profile", filepath.Join(testdata, "profiles", "csv_refs.yaml"), "--out", out}, &stdout, &stderr)).To(Equal(0), stderr.String())
		Expect(out).To(BeAnExistingFile())
		want, err := filepath.Abs(filepath.Join(synthRoot, "test", "eval", "crosswalk", "testdata", "synthetic_catalog.json"))
		Expect(err).NotTo(HaveOccurred())
		Expect(stdout.String()).To(Equal(want + "\n"))
	})

	It("scores a run, writes report.json and report.md, and prints the summary line", func() {
		out := filepath.Join(GinkgoT().TempDir(), "report")
		runDir := filepath.Join(testdata, "run")
		args := []string{
			"score", "--root", synthRoot,
			"--profile", filepath.Join(testdata, "profiles", "links_in_text.yaml"),
			"--subset", filepath.Join(testdata, "source_a.json"),
			"--mappings", filepath.Join(runDir, "mappings.json"),
			"--candidates", filepath.Join(runDir, "candidates.json"),
			"--status", filepath.Join(runDir, "status.json"),
			"--out", out,
		}
		Expect(run(args, &stdout, &stderr)).To(Equal(0), stderr.String())
		Expect(stdout.String()).To(Equal("synth-links-in-text unlabelled: refs 3 (resolved 3/3), found exact 1/3 base 2/3, " +
			"reachable exact 2/3 base 3/3, related 2, chat calls 2, wall 4.0s\n"))
		Expect(filepath.Join(out, "report.json")).To(BeAnExistingFile())
		Expect(filepath.Join(out, "report.md")).To(BeAnExistingFile())
	})

	Describe("--labels", func() {
		scoreArgs := func(out string, extra ...string) []string {
			runDir := filepath.Join(testdata, "run")
			return append([]string{
				"score", "--root", synthRoot,
				"--profile", filepath.Join(testdata, "profiles", "links_in_text.yaml"),
				"--subset", filepath.Join(testdata, "source_a.json"),
				"--mappings", filepath.Join(runDir, "mappings.json"),
				"--candidates", filepath.Join(runDir, "candidates.json"),
				"--status", filepath.Join(runDir, "status.json"),
				"--out", out,
			}, extra...)
		}

		It("adds labelled precision to the report", func() {
			out := filepath.Join(GinkgoT().TempDir(), "report")
			labels := filepath.Join(GinkgoT().TempDir(), "labels.yaml")
			Expect(os.WriteFile(labels, []byte("version: 1\npairs:\n  - {source: s-1, target: ab-2, verdict: related, judged_by: [opus], rationale: fixture, date: 2026-11-12}\n"), 0o600)).To(Succeed())
			Expect(run(scoreArgs(out, "--labels", labels), &stdout, &stderr)).To(Equal(0), stderr.String())
			md, err := os.ReadFile(filepath.Join(out, "report.md"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(md)).To(ContainSubstring("Labelled precision: 1/1 (100.0%)"))
			Expect(string(md)).To(MatchRegexp(`- Labels SHA-256: [0-9a-f]{64}\n`))
		})

		It("fails on an invalid labels file, naming it, and writes no report", func() {
			out := filepath.Join(GinkgoT().TempDir(), "report")
			labels := filepath.Join(GinkgoT().TempDir(), "labels.yaml")
			Expect(os.WriteFile(labels, []byte("version: 2\n"), 0o600)).To(Succeed())
			Expect(run(scoreArgs(out, "--labels", labels), &stdout, &stderr)).NotTo(Equal(0))
			Expect(stderr.String()).To(ContainSubstring(labels))
			Expect(filepath.Join(out, "report.md")).NotTo(BeAnExistingFile())
		})
	})
})
