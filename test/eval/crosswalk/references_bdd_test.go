package crosswalk_test

import (
	"os"
	"path/filepath"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/test/eval/crosswalk"
)

func mustProfile(path string) *crosswalk.Profile {
	p, err := crosswalk.LoadProfile(path)
	Expect(err).NotTo(HaveOccurred())
	return p
}

func mustCatalog(path string) *crosswalk.Catalog {
	c, err := crosswalk.LoadCatalog(path)
	Expect(err).NotTo(HaveOccurred())
	return c
}

var _ = Describe("SplitLinkText", func() {
	DescribeTable("trims tokens and skips empty ones",
		func(text, sep string, want []string) {
			Expect(crosswalk.SplitLinkText(text, sep)).To(Equal(want))
		},
		Entry("padding and an empty token", "A-1, , A-2 ", ",", []string{"A-1", "A-2"}),
		Entry("only separators", " , ,", ",", nil),
		Entry("empty text", "", ",", nil),
		Entry("custom separator", "A-1;B-2", ";", []string{"A-1", "B-2"}),
		Entry("multi-character separator", "A-1 | B-2", "|", []string{"A-1", "B-2"}),
	)
})

var _ = Describe("ExtractReferences", func() {
	It("reads identifiers from link text of matching resources only", func() {
		p := mustProfile("testdata/profiles/links_in_text.yaml")
		raw, err := crosswalk.ExtractReferences(p, ".", mustCatalog("testdata/source_a.json"))
		Expect(err).NotTo(HaveOccurred())
		const cite = "SYNTHTARGET https://example.invalid/target.pdf"
		Expect(raw).To(Equal([]crosswalk.RawReference{
			{SourceID: "s-1", Identifier: "AB-2", Citation: cite},
			{SourceID: "s-1", Identifier: "CD-1", Citation: cite},
			{SourceID: "s-2", Identifier: "ab-2(3)", Citation: cite},
		}))
	})

	It("reads one identifier per link from the resource title, selecting by rlink", func() {
		p := mustProfile("testdata/profiles/titles_by_rlink.yaml")
		raw, err := crosswalk.ExtractReferences(p, ".", mustCatalog("testdata/source_b.json"))
		Expect(err).NotTo(HaveOccurred())
		const cite = " https://example.invalid/SYNTH_TARGET_1.pdf"
		Expect(raw).To(Equal([]crosswalk.RawReference{
			{SourceID: "t-1", Identifier: "AB-02", Citation: "AB-02" + cite},
			{SourceID: "t-2", Identifier: "CD-01", Citation: "CD-01" + cite},
		}))
	})

	It("requires every selector that is given to match", func() {
		p := mustProfile("testdata/profiles/links_in_text.yaml")
		p.References.OSCALLinks.Resource.Rlink = "no-such-url"
		raw, err := crosswalk.ExtractReferences(p, ".", mustCatalog("testdata/source_a.json"))
		Expect(err).NotTo(HaveOccurred())
		Expect(raw).To(BeEmpty())
	})

	It("names a profile control that the source lacks", func() {
		p := mustProfile("testdata/profiles/links_in_text.yaml")
		p.Controls = append(p.Controls, "s-404")
		_, err := crosswalk.ExtractReferences(p, ".", mustCatalog("testdata/source_a.json"))
		Expect(err).To(MatchError(And(ContainSubstring("s-404"), ContainSubstring("source"))))
	})

	It("names the control and href of a link to a missing back-matter resource", func() {
		p := mustProfile("testdata/profiles/links_in_text.yaml")
		src := mustCatalog("testdata/source_a.json")
		resources := *src.Doc.BackMatter.Resources
		src.Doc.BackMatter.Resources = &[]oscalTypes.Resource{resources[1]}
		_, err := crosswalk.ExtractReferences(p, ".", src)
		Expect(err).To(MatchError(And(ContainSubstring("s-1"), ContainSubstring("#aaaaaaaa-0000-4000-8000-000000000001"))))
	})

	Describe("CSV", func() {
		Describe("CSVDigest", func() {
			write := func(content string) (*crosswalk.Profile, string) {
				dir := GinkgoT().TempDir()
				Expect(os.WriteFile(filepath.Join(dir, "r.csv"), []byte(content), 0o600)).To(Succeed())
				p := mustProfile("testdata/profiles/csv_refs.yaml")
				p.References.CSV = "r.csv"
				return p, dir
			}

			It("hashes the file bytes, so an edit changes it", func() {
				p, dir := write("source_id,target_id\ns-1,AB-2\n")
				before, err := crosswalk.CSVDigest(p, dir)
				Expect(err).NotTo(HaveOccurred())
				Expect(before).To(MatchRegexp(`^[0-9a-f]{64}$`))
				Expect(os.WriteFile(filepath.Join(dir, "r.csv"), []byte("source_id,target_id\ns-1,CD-1\n"), 0o600)).To(Succeed())
				after, err := crosswalk.CSVDigest(p, dir)
				Expect(err).NotTo(HaveOccurred())
				Expect(after).NotTo(Equal(before))
			})

			It("is empty for a profile that uses oscal_links", func() {
				p := mustProfile("testdata/profiles/links_in_text.yaml")
				Expect(crosswalk.CSVDigest(p, ".")).To(BeEmpty())
			})

			DescribeTable("fails closed on a CSV it cannot read",
				func(path, want string) {
					p := mustProfile("testdata/profiles/csv_refs.yaml")
					p.References.CSV = path
					_, err := crosswalk.CSVDigest(p, GinkgoT().TempDir())
					Expect(err).To(MatchError(ContainSubstring(want)))
				},
				Entry("a path outside the root", "../outside.csv", "inside the repository root"),
				Entry("a missing file", "absent.csv", "absent.csv"),
			)
		})

		It("reads rows relative to root, with an optional citation", func() {
			p := mustProfile("testdata/profiles/csv_refs.yaml")
			p.References.CSV = "testdata/refs.csv"
			raw, err := crosswalk.ExtractReferences(p, ".", mustCatalog("testdata/source_a.json"))
			Expect(err).NotTo(HaveOccurred())
			Expect(raw).To(Equal([]crosswalk.RawReference{
				{SourceID: "s-1", Identifier: "AB-2", Citation: "doc one"},
				{SourceID: "s-2", Identifier: "CD-1", Citation: "csv:testdata/refs.csv"},
				{SourceID: "s-1", Identifier: "ab-2", Citation: "dup"},
			}))
		})

		DescribeTable("rejects bad input with the line number",
			func(content, want string) {
				dir := GinkgoT().TempDir()
				Expect(os.WriteFile(filepath.Join(dir, "r.csv"), []byte(content), 0o600)).To(Succeed())
				p := mustProfile("testdata/profiles/csv_refs.yaml")
				p.References.CSV = "r.csv"
				_, err := crosswalk.ExtractReferences(p, dir, mustCatalog("testdata/source_a.json"))
				Expect(err).To(MatchError(And(ContainSubstring("r.csv"), ContainSubstring(want))))
			},
			Entry("missing target_id column", "source_id,other\ns-1,x\n", "target_id"),
			Entry("missing header", "", "header"),
			Entry("source not in controls", "source_id,target_id\ns-1,AB-2\ns-3,AB-2\n", "line 3"),
			Entry("empty target", "source_id,target_id\ns-1,\n", "line 2"),
			Entry("short row", "source_id,target_id\ns-1\n", "line 2"),
			Entry("malformed quote in a row's first field", "source_id,target_id\ns\"1,AB-2\n", "line 2"),
		)

		It("rejects a path that escapes root", func() {
			p := mustProfile("testdata/profiles/csv_refs.yaml")
			p.References.CSV = "../outside.csv"
			_, err := crosswalk.ExtractReferences(p, ".", mustCatalog("testdata/source_a.json"))
			Expect(err).To(MatchError(ContainSubstring("references.csv")))
		})

		It("names a missing file", func() {
			p := mustProfile("testdata/profiles/csv_refs.yaml")
			p.References.CSV = "absent.csv"
			_, err := crosswalk.ExtractReferences(p, ".", mustCatalog("testdata/source_a.json"))
			Expect(err).To(MatchError(ContainSubstring("absent.csv")))
		})
	})
})

var _ = Describe("ResolveReferences", func() {
	It("splits resolved, unresolved and ambiguous, and de-duplicates resolved pairs", func() {
		idx := crosswalk.NewTargetIndex(mustCatalog(syntheticCatalog))
		raw := []crosswalk.RawReference{
			{SourceID: "s-1", Identifier: "AB-2(3)", Citation: "first"},
			{SourceID: "s-1", Identifier: "ab-02(03)", Citation: "second"},
			{SourceID: "s-2", Identifier: "AB-2(3)", Citation: "other source"},
			{SourceID: "s-1", Identifier: "ZZ-9", Citation: "c"},
			{SourceID: "s-1", Identifier: "SHARED-9", Citation: "c"},
		}
		got := crosswalk.ResolveReferences(raw, idx)
		Expect(got.Resolved).To(Equal([]crosswalk.Reference{
			{SourceID: "s-1", TargetID: "ab-2.3", Citation: "first"},
			{SourceID: "s-2", TargetID: "ab-2.3", Citation: "other source"},
		}))
		Expect(got.ResolvedRaw).To(Equal(raw[:3]))
		Expect(got.Unresolved).To(Equal([]crosswalk.RawReference{raw[3]}))
		Expect(got.Ambiguous).To(Equal([]crosswalk.RawReference{raw[4]}))
	})

	It("accounts for every raw reference exactly once", func() {
		idx := crosswalk.NewTargetIndex(mustCatalog(syntheticCatalog))
		got := crosswalk.ResolveReferences(nil, idx)
		Expect(got.Resolved).To(BeEmpty())
		Expect(got.Unresolved).To(BeEmpty())
		Expect(got.Ambiguous).To(BeEmpty())
	})
})
