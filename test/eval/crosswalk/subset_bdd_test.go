package crosswalk_test

import (
	"encoding/json"
	"os"
	"path/filepath"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/test/eval/crosswalk"
)

func ids(controls *[]oscalTypes.Control) []string {
	var out []string
	if controls != nil {
		for _, c := range *controls {
			out = append(out, c.ID)
		}
	}
	return out
}

var _ = Describe("Subset", func() {
	var src *crosswalk.Catalog

	BeforeEach(func() { src = mustCatalog(syntheticCatalog) })

	It("keeps each listed control's ancestor chain and drops unlisted children", func() {
		out, err := crosswalk.Subset(src, []string{"ab-2.3.1", "cd-9"})
		Expect(err).NotTo(HaveOccurred())
		Expect(*out.Groups).To(HaveLen(2))
		ab := (*out.Groups)[0]
		Expect(ab.ID).To(Equal("ab"))
		Expect(ids(ab.Controls)).To(Equal([]string{"ab-2"}))
		parent := (*ab.Controls)[0]
		Expect(ids(parent.Controls)).To(Equal([]string{"ab-2.3"}))
		Expect(ids((*parent.Controls)[0].Controls)).To(Equal([]string{"ab-2.3.1"}))
		Expect(ids((*out.Groups)[1].Controls)).To(Equal([]string{"cd-9"}))
	})

	It("drops unlisted controls and groups left empty", func() {
		out, err := crosswalk.Subset(src, []string{"cd-1"})
		Expect(err).NotTo(HaveOccurred())
		Expect(*out.Groups).To(HaveLen(1))
		Expect((*out.Groups)[0].ID).To(Equal("cd"))
		Expect(ids((*out.Groups)[0].Controls)).To(Equal([]string{"cd-1"}))
		Expect(out.Controls).To(BeNil())
	})

	Describe("with a catalog whose controls have parents", func() {
		var ssdf *crosswalk.Catalog

		BeforeEach(func() { ssdf = mustCatalog(ssdfStyleCatalog) })

		// control finds id anywhere under controls.
		var control func(controls *[]oscalTypes.Control, id string) *oscalTypes.Control
		control = func(controls *[]oscalTypes.Control, id string) *oscalTypes.Control {
			if controls == nil {
				return nil
			}
			for i, c := range *controls {
				if c.ID == id {
					return &(*controls)[i]
				}
				if found := control(c.Controls, id); found != nil {
					return found
				}
			}
			return nil
		}

		// find returns the control id anywhere under the groups' controls.
		find := func(groups *[]oscalTypes.Group, id string) *oscalTypes.Control {
			for _, g := range *groups {
				if c := control(g.Controls, id); c != nil {
					return c
				}
			}
			return nil
		}

		It("keeps the parent of a listed control with its params, parts and labels", func() {
			out, err := crosswalk.Subset(ssdf, []string{"PO.1.2"})
			Expect(err).NotTo(HaveOccurred())
			Expect(*out.Groups).To(HaveLen(1))
			po := (*out.Groups)[0]
			Expect(po.ID).To(Equal("PO"))
			Expect(ids(po.Controls)).To(Equal([]string{"PO.1"}))
			parent := (*po.Controls)[0]
			Expect(parent.Params).NotTo(BeNil())
			Expect(*parent.Params).To(HaveLen(1))
			Expect((*parent.Params)[0].ID).To(Equal("PO.1_prm_1"))
			Expect(parent.Parts).NotTo(BeNil())
			Expect(parent.Props).NotTo(BeNil())
			Expect(ids(parent.Controls)).To(Equal([]string{"PO.1.2"}))
		})

		It("marks only the listed controls as in scope", func() {
			out, err := crosswalk.Subset(ssdf, []string{"PO.1.2", "PS.1"})
			Expect(err).NotTo(HaveOccurred())
			scoped := func(c *oscalTypes.Control) bool {
				if c.Props == nil {
					return false
				}
				for _, p := range *c.Props {
					if p.Name == crosswalk.ScopeProp && p.Value == crosswalk.ScopeIn {
						return true
					}
				}
				return false
			}
			Expect(scoped(find(out.Groups, "PO.1.2"))).To(BeTrue())
			Expect(scoped(find(out.Groups, "PS.1"))).To(BeTrue())
			Expect(scoped(find(out.Groups, "PO.1"))).To(BeFalse())
			// The unlisted sibling is dropped.
			Expect(find(out.Groups, "PO.1.1")).To(BeNil())
			Expect(find(out.Groups, "PO.2")).To(BeNil())
		})

		It("leaves the source catalog without scope marks", func() {
			_, err := crosswalk.Subset(ssdf, []string{"PO.1.2"})
			Expect(err).NotTo(HaveOccurred())
			Expect(ssdf.Doc).To(Equal(mustCatalog(ssdfStyleCatalog).Doc))
		})

		It("keeps a listed parent and a listed child together", func() {
			out, err := crosswalk.Subset(ssdf, []string{"PO.1", "PO.1.1"})
			Expect(err).NotTo(HaveOccurred())
			parent := find(out.Groups, "PO.1")
			Expect(ids(parent.Controls)).To(Equal([]string{"PO.1.1"}))
		})
	})

	It("keeps a top-level control at the top level", func() {
		out, err := crosswalk.Subset(src, []string{"top.1"})
		Expect(err).NotTo(HaveOccurred())
		Expect(out.Groups).To(BeNil())
		Expect(ids(out.Controls)).To(Equal([]string{"top.1"}))
	})

	It("keeps listed controls at the top level when the catalog has no groups", func() {
		out, err := crosswalk.Subset(mustCatalog("testdata/source_a.json"), []string{"s-2", "s-1"})
		Expect(err).NotTo(HaveOccurred())
		Expect(out.Groups).To(BeNil())
		Expect(ids(out.Controls)).To(Equal([]string{"s-1", "s-2"}))
	})

	It("leaves back-matter unchanged", func() {
		a := mustCatalog("testdata/source_a.json")
		out, err := crosswalk.Subset(a, []string{"s-1"})
		Expect(err).NotTo(HaveOccurred())
		Expect(out.BackMatter).To(Equal(a.Doc.BackMatter))
	})

	It("marks the title, derives a new UUID and keeps last-modified", func() {
		out, err := crosswalk.Subset(src, []string{"cd-1"})
		Expect(err).NotTo(HaveOccurred())
		Expect(out.Metadata.Title).To(Equal("Synthetic (eval subset)"))
		Expect(out.UUID).NotTo(Equal(src.Doc.UUID))
		Expect(out.Metadata.LastModified).To(Equal(src.Doc.Metadata.LastModified))
	})

	It("gives byte-identical JSON for the same source and controls, in any order", func() {
		a, err := crosswalk.Subset(src, []string{"cd-1", "ab-2.3"})
		Expect(err).NotTo(HaveOccurred())
		b, err := crosswalk.Subset(mustCatalog(syntheticCatalog), []string{"ab-2.3", "cd-1"})
		Expect(err).NotTo(HaveOccurred())
		rawA, err := json.Marshal(a)
		Expect(err).NotTo(HaveOccurred())
		rawB, err := json.Marshal(b)
		Expect(err).NotTo(HaveOccurred())
		Expect(rawA).To(Equal(rawB))
	})

	It("gives a different UUID for different controls", func() {
		a, err := crosswalk.Subset(src, []string{"cd-1"})
		Expect(err).NotTo(HaveOccurred())
		b, err := crosswalk.Subset(src, []string{"cd-9"})
		Expect(err).NotTo(HaveOccurred())
		Expect(a.UUID).NotTo(Equal(b.UUID))
	})

	It("does not modify the source catalog", func() {
		before := mustCatalog(syntheticCatalog)
		_, err := crosswalk.Subset(src, []string{"ab-2.3"})
		Expect(err).NotTo(HaveOccurred())
		Expect(src.Doc).To(Equal(before.Doc))
	})

	It("lists every unknown control in the error", func() {
		_, err := crosswalk.Subset(src, []string{"zz-9", "cd-1", "aa-1"})
		Expect(err).To(MatchError(And(ContainSubstring("aa-1, zz-9"), ContainSubstring(syntheticCatalog), Not(ContainSubstring("cd-1,")), ContainSubstring("profile's controls list"))))
	})

	It("writes a subset that survives a JSON round trip", func() {
		out, err := crosswalk.Subset(src, []string{"cd-1"})
		Expect(err).NotTo(HaveOccurred())
		path := filepath.Join(GinkgoT().TempDir(), "subset.json")
		raw, err := json.Marshal(map[string]any{"catalog": out})
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(path, raw, 0o600)).To(Succeed())
		back := mustCatalog(path)
		Expect(back.Controls()).To(HaveLen(1))
	})
})
