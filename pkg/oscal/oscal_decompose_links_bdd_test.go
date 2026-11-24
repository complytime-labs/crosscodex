package oscal_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

// splittable is long enough to decompose at the default minimum and has a
// lettered list whose first entry has numbered sub-items.
const splittable = "(a) The first clause has these parts which are listed below in order:\n" +
	"1. A first numbered sub-requirement long enough to count as its own item here.\n" +
	"2. A second numbered sub-requirement long enough to count as its own item here.\n" +
	"(b) The second clause stands alone and is long enough to be kept as one item."

var _ = Describe("Structurer decomposition", func() {
	decomposeOpts := oscal.StructureOptions{Decompose: true, MinDecomposeWords: 5}

	DescribeTable("keeps an unsplit item's title, links and props",
		func(id, parent, group string) {
			in := oscal.ControlItem{ID: id, Title: "A title", Text: "  Too short to split.  ", Class: oscal.ClassRequirement,
				ParentID: parent, GroupID: group, Props: map[string]string{"label": "x"}}

			out := oscal.ExportApplyDecomposition([]oscal.ControlItem{in}, decomposeOpts)

			want := in
			want.Text = "Too short to split."
			Expect(out).To(Equal([]oscal.ControlItem{want}))
		},
		Entry("family-dash-number IDs", "FA-01(02)", "FA-01", "fam-a"),
		Entry("dotted zero-padded IDs", "07.03.01.b", "07.03.01", "07.03"),
	)

	DescribeTable("gives split sub-items the item's title and group, and its parent to the top-level ones",
		func(id, parent, group string) {
			in := oscal.ControlItem{ID: id, Title: "A title", Text: splittable, Class: oscal.ClassRequirement,
				ParentID: parent, GroupID: group}

			out := oscal.ExportApplyDecomposition([]oscal.ControlItem{in}, decomposeOpts)

			Expect(out).To(HaveLen(4))
			Expect(out).To(HaveEach(And(HaveField("Title", "A title"), HaveField("GroupID", group))))
			Expect(out[0]).To(And(HaveField("ID", id+".a"), HaveField("ParentID", parent), HaveField("Class", oscal.ClassSection)))
			Expect(out[1]).To(And(HaveField("ID", id+".a.1"), HaveField("ParentID", id+".a")))
			Expect(out[2]).To(And(HaveField("ID", id+".a.2"), HaveField("ParentID", id+".a")))
			Expect(out[3]).To(And(HaveField("ID", id+".b"), HaveField("ParentID", parent)))
		},
		Entry("family-dash-number IDs", "FA-01", "", "fam-a"),
		Entry("dotted zero-padded IDs, under a parent", "07.03.01.b", "07.03.01", "07.03"),
	)
})

var _ = Describe("DecomposeText label clashes", func() {
	DescribeTable("numbers a label that restarts within one text, keeping the published label",
		func(base string) {
			text := "(a) First entry of the first list.\n(b) Second entry of the first list.\n" +
				"Some prose between the two lists.\n" +
				"(a) First entry of the second list.\n(b) Second entry of the second list."

			out := oscal.DecomposeText(base, text, 3)

			ids := make([]string, len(out))
			labels := make([]string, len(out))
			for i, item := range out {
				ids[i], labels[i] = item.ID, item.Props["label"]
			}
			Expect(ids).To(Equal([]string{base + ".a", base + ".b", base + ".a-2", base + ".b-2"}))
			Expect(labels).To(Equal([]string{"a", "b", "a", "b"}))
		},
		Entry("family-dash-number IDs", "FA-01"),
		Entry("dotted zero-padded IDs", "07.03.01"),
	)
})
