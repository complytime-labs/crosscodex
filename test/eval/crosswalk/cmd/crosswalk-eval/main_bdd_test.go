package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

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

	Describe("label-input", func() {
		runWith := func(controls string) string {
			dir := GinkgoT().TempDir()
			cand := `{"job_id":"j","candidates":[{"sourceControlId":"c-s/a","targetControlId":"c-t/b"}]}`
			for name, content := range map[string]string{"candidates.json": cand, "controls-source.json": controls, "controls-target.json": `{"catalog_id":"c-t","controls":[{"control_id":"b","title":"B","statement":"line\nbreak"}]}`} {
				Expect(os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600)).To(Succeed())
			}
			return dir
		}

		It("prints one JSON line per candidate pair", func() {
			dir := runWith(`{"catalog_id":"c-s","controls":[{"control_id":"a","title":"A","statement":"say \"x\""}]}`)
			Expect(run([]string{"label-input", "--run", dir}, &stdout, &stderr)).To(Equal(0), stderr.String())
			Expect(stdout.String()).To(Equal(`{"source":"a","target":"b","source_title":"A","source_text":"say \"x\"","target_title":"B","target_text":"line\nbreak"}` + "\n"))
		})

		It("exits 1 naming the missing control and prints nothing", func() {
			dir := runWith(`{"catalog_id":"c-s","controls":[]}`)
			Expect(run([]string{"label-input", "--run", dir}, &stdout, &stderr)).To(Equal(1))
			Expect(stderr.String()).To(ContainSubstring(`source control "a"`))
			Expect(stdout.Len()).To(BeZero())
		})

		DescribeTable("usage errors",
			func(args []string, stderrHas string) {
				Expect(run(append([]string{"label-input"}, args...), &stdout, &stderr)).To(Equal(2))
				Expect(stderr.String()).To(ContainSubstring(stderrHas))
			},
			Entry("no --run", nil, "missing required flag --run"),
			Entry("unknown flag", []string{"--nope"}, "flag provided but not defined"),
			Entry("stray argument", []string{"--run", "d", "extra"}, `unexpected argument "extra"`),
		)
	})
})

var _ = Describe("labels merge", func() {
	var (
		stdout, stderr bytes.Buffer
		dir            string
	)

	BeforeEach(func() {
		stdout.Reset()
		stderr.Reset()
		dir = GinkgoT().TempDir()
		clock = func() time.Time { return time.Date(2026, 11, 24, 23, 59, 0, 0, time.UTC) }
		DeferCleanup(func() { clock = time.Now })
	})

	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		Expect(os.WriteFile(path, []byte(content), 0o600)).To(Succeed())
		return path
	}
	pairs := func(ids ...string) string {
		var s string
		for _, id := range ids {
			s += fmt.Sprintf(`{"source":%q,"target":"t","source_title":"x"}`+"\n", id)
		}
		return s
	}
	judged := func(verdicts map[string]string) string {
		var s string
		for id, v := range verdicts {
			s += fmt.Sprintf(`{"source":%q,"target":"t","verdict":%q,"rationale":"because"}`+"\n", id, v)
		}
		return s
	}

	It("merges, stamps today's UTC date and prints the disputed pairs as source and target", func() {
		pairsFile := write("pairs.jsonl", pairs("a", "b"))
		opus := write("opus.jsonl", judged(map[string]string{"a": "related", "b": "related"}))
		sonnet := write("sonnet.jsonl", judged(map[string]string{"a": "related", "b": "unrelated"}))
		out := filepath.Join(dir, "labels.yaml")
		code := run([]string{"labels", "merge", "--pairs", pairsFile, "--judge", "opus=" + opus, "--judge", "sonnet=" + sonnet, "--out", out}, &stdout, &stderr)
		Expect(code).To(Equal(0), stderr.String())
		Expect(stdout.String()).To(Equal("b\tt\n"))
		raw, err := os.ReadFile(out)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(ContainSubstring("2026-11-24"))
	})

	It("exits 1, prints nothing and leaves --out byte-identical when a judge misses a pair", func() {
		pairsFile := write("pairs.jsonl", pairs("a", "b"))
		opus := write("opus.jsonl", judged(map[string]string{"a": "related"}))
		out := write("labels.yaml", "prior\n")
		code := run([]string{"labels", "merge", "--pairs", pairsFile, "--judge", "opus=" + opus, "--out", out}, &stdout, &stderr)
		Expect(code).To(Equal(1))
		Expect(stderr.String()).To(ContainSubstring("b -> t"))
		Expect(stdout.String()).To(BeEmpty())
		raw, err := os.ReadFile(out)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(Equal("prior\n"))
	})

	It("exits 1 for an --input that does not exist", func() {
		pairsFile := write("pairs.jsonl", pairs("a"))
		opus := write("opus.jsonl", judged(map[string]string{"a": "related"}))
		out := filepath.Join(dir, "labels.yaml")
		code := run([]string{"labels", "merge", "--input", filepath.Join(dir, "absent.yaml"), "--pairs", pairsFile, "--judge", "opus=" + opus, "--out", out}, &stdout, &stderr)
		Expect(code).To(Equal(1))
		Expect(stderr.String()).To(ContainSubstring("absent.yaml"))
		Expect(out).NotTo(BeAnExistingFile())
	})

	It("exits 1 naming a --judge file that does not exist and leaves --out byte-identical", func() {
		pairsFile := write("pairs.jsonl", pairs("a"))
		absent := filepath.Join(dir, "absent-judge.jsonl")
		out := write("labels.yaml", "prior\n")
		code := run([]string{"labels", "merge", "--pairs", pairsFile, "--judge", "opus=" + absent, "--out", out}, &stdout, &stderr)
		Expect(code).To(Equal(1))
		Expect(stderr.String()).To(ContainSubstring(absent))
		Expect(stdout.String()).To(BeEmpty())
		raw, err := os.ReadFile(out)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(Equal("prior\n"))
	})

	DescribeTable("usage errors",
		func(args []string, stderrHas string) {
			Expect(run(append([]string{"labels", "merge"}, args...), &stdout, &stderr)).To(Equal(2))
			Expect(stderr.String()).To(ContainSubstring(stderrHas))
		},
		Entry("no --pairs", []string{"--judge", "opus=f", "--out", "o"}, "missing required flag --pairs"),
		Entry("no --out", []string{"--pairs", "p", "--judge", "opus=f"}, "missing required flag --out"),
		Entry("no --judge", []string{"--pairs", "p", "--out", "o"}, "at least one --judge"),
		Entry("a --judge without a file", []string{"--pairs", "p", "--out", "o", "--judge", "opus"}, "NAME=FILE"),
		Entry("a --judge named human", []string{"--pairs", "p", "--out", "o", "--judge", "human=f"}, "opus, sonnet"),
		Entry("a --judge named twice", []string{"--pairs", "p", "--out", "o", "--judge", "opus=f", "--judge", "opus=g"}, `"opus" is given twice`),
		Entry("an unknown flag", []string{"--nope"}, "flag provided but not defined"),
		Entry("a stray argument", []string{"--pairs", "p", "--out", "o", "--judge", "opus=f", "extra"}, `unexpected argument "extra"`),
	)

	DescribeTable("usage errors above merge",
		func(args []string, stderrHas string) {
			Expect(run(args, &stdout, &stderr)).To(Equal(2))
			Expect(stderr.String()).To(ContainSubstring(stderrHas))
		},
		Entry("no labels subcommand", []string{"labels"}, "Usage: crosswalk-eval labels merge"),
		Entry("unknown labels subcommand", []string{"labels", "bogus"}, `unknown labels subcommand "bogus"`),
	)
})
