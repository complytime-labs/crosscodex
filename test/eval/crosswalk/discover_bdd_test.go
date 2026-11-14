package crosswalk_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gstruct"

	"github.com/complytime-labs/crosscodex/test/eval/crosswalk"
)

func findRow(rows []crosswalk.DiscoverRow, title, rel, mode string) crosswalk.DiscoverRow {
	for _, r := range rows {
		if r.ResourceTitle == title && r.Rel == rel && r.Mode == mode {
			return r
		}
	}
	Fail("no discover row for " + title + "/" + rel + "/" + mode)
	return crosswalk.DiscoverRow{}
}

var _ = Describe("Discover", func() {
	var (
		src *crosswalk.Catalog
		idx *crosswalk.TargetIndex
	)

	BeforeEach(func() {
		src = mustCatalog("testdata/source_a.json")
		idx = crosswalk.NewTargetIndex(mustCatalog(syntheticCatalog))
		// Make the decoy cite identifiers that mostly do not exist in the target.
		decoy := *src.Doc.Controls
		(*decoy[0].Links)[1].Text = "ZZ-1, CD-9"
	})

	It("scores the real reference resource 100% in link_text mode", func() {
		rows := crosswalk.Discover(src, idx)
		r := findRow(rows, "SYNTHTARGET", "x_ref", "link_text")
		Expect(r).To(Equal(crosswalk.DiscoverRow{
			Rel: "x_ref", ResourceUUID: "aaaaaaaa-0000-4000-8000-000000000001", ResourceTitle: "SYNTHTARGET",
			Mode: "link_text", Links: 3, Identifiers: 4, Resolved: 4,
		}))
	})

	It("scores a decoy resource lower", func() {
		r := findRow(crosswalk.Discover(src, idx), "DECOYDOC", "x_ref", "link_text")
		Expect(r.Identifiers).To(Equal(2))
		Expect(r.Resolved).To(Equal(1))
	})

	It("reports both modes for every (rel, resource)", func() {
		rows := crosswalk.Discover(src, idx)
		Expect(rows).To(HaveLen(6)) // (x_ref, SYNTHTARGET), (x_ref, DECOYDOC), (see, SYNTHTARGET) x 2 modes
		title := findRow(rows, "SYNTHTARGET", "x_ref", "resource_title")
		Expect(title).To(MatchFields(IgnoreExtras, Fields{"Links": Equal(3), "Identifiers": Equal(3), "Resolved": Equal(0)}))
	})

	It("sorts by resolved share descending, then title", func() {
		rows := crosswalk.Discover(src, idx)
		Expect(rows[0].ResourceTitle).To(Equal("SYNTHTARGET"))
		Expect(rows[0].Mode).To(Equal("link_text"))
		Expect(rows[len(rows)-1].Resolved).To(Equal(0))
	})

	It("skips links whose resource is not in the back-matter", func() {
		(*(*src.Doc.Controls)[1].Links)[0].Href = "#missing"
		rows := crosswalk.Discover(src, idx)
		Expect(findRow(rows, "SYNTHTARGET", "x_ref", "link_text").Links).To(Equal(2))
	})

	It("counts resource_title identifiers that resolve", func() {
		b := mustCatalog("testdata/source_b.json")
		r := findRow(crosswalk.Discover(b, idx), "AB-02", "reference", "resource_title")
		Expect(r).To(MatchFields(IgnoreExtras, Fields{"Links": Equal(2), "Identifiers": Equal(2), "Resolved": Equal(2)}))
	})
})
