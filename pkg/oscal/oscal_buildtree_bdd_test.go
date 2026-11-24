package oscal_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

// idEncoding builds item IDs in one catalog's style, so BuildTree specs run
// against identifiers it must not interpret: a family-dash-number encoding
// with a parenthesized enhancement, and a dotted zero-padded one.
type idEncoding struct {
	group, control, enhancement, otherGroup, otherControl string
}

var buildTreeEncodings = []TableEntry{
	Entry("family-dash-number IDs", idEncoding{
		group: "fam-a", control: "FA-01", enhancement: "FA-01(02)", otherGroup: "fam-b", otherControl: "FB-07",
	}),
	Entry("dotted zero-padded IDs", idEncoding{
		group: "07.03", control: "07.03.01", enhancement: "07.03.01.b", otherGroup: "07.04", otherControl: "07.04.11",
	}),
}

var _ = Describe("BuildTree", func() {
	DescribeTable("groups items by their GroupID and keeps ParentID links",
		func(ids idEncoding) {
			items := []oscal.ControlItem{
				{ID: ids.group, Title: "First family", Text: "Family prose.", Class: oscal.ClassSection, Props: map[string]string{"label": "1"}},
				{ID: ids.control, Title: "A control", Text: "The organization shall act.", Class: oscal.ClassRequirement, GroupID: ids.group},
				{ID: ids.enhancement, Text: "And more.", Class: oscal.ClassRequirement, ParentID: ids.control},
				{ID: ids.otherGroup, Title: "Second family", Class: oscal.ClassSection, GroupID: ids.group},
				{ID: ids.otherControl, Title: "Another control", Text: "It shall also act.", Class: oscal.ClassRequirement, GroupID: ids.otherGroup},
			}

			tree, err := oscal.BuildTree(items)
			Expect(err).NotTo(HaveOccurred())

			Expect(tree.Containers).To(Equal([]oscal.Container{
				{Key: "g1", Identifier: ids.group, Title: "First family", Label: "1", Prose: "Family prose."},
				{Key: "g1.1", Identifier: ids.otherGroup, Title: "Second family", ParentKey: "g1"},
			}))
			Expect(tree.Items).To(HaveLen(3))
			Expect(tree.Items[0]).To(haveLinks(ids.control, "", ids.group, "g1"))
			Expect(tree.Items[1]).To(haveLinks(ids.enhancement, ids.control, "", "g1"))
			Expect(tree.Items[2]).To(haveLinks(ids.otherControl, "", ids.otherGroup, "g1.1"))
		},
		buildTreeEncodings,
	)

	DescribeTable("orders top-level and sibling containers by first appearance",
		func(ids idEncoding) {
			items := []oscal.ControlItem{
				{ID: ids.otherGroup, Title: "Shown first", Class: oscal.ClassSection},
				{ID: ids.group, Title: "Shown second", Class: oscal.ClassSection},
				{ID: ids.control, Text: "x", GroupID: ids.group},
				{ID: ids.otherControl, Text: "y", GroupID: ids.otherGroup},
			}
			tree, err := oscal.BuildTree(items)
			Expect(err).NotTo(HaveOccurred())
			Expect(tree.Containers).To(HaveLen(2))
			Expect(tree.Containers[0].Identifier).To(Equal(ids.otherGroup))
			Expect(tree.Containers[0].Key).To(Equal("g1"))
			Expect(tree.Containers[1].Identifier).To(Equal(ids.group))
			Expect(tree.Containers[1].Key).To(Equal("g2"))
		},
		buildTreeEncodings,
	)

	It("returns flat items unchanged and no containers when no item names a group", func() {
		items := []oscal.ControlItem{
			{ID: "sec-1", Title: "One", Text: "a", Class: oscal.ClassRequirement},
			{ID: "sec-1.a", Text: "b", Class: oscal.ClassRequirement, ParentID: "sec-1"},
		}
		tree, err := oscal.BuildTree(items)
		Expect(err).NotTo(HaveOccurred())
		Expect(tree.Containers).To(BeEmpty())
		Expect(tree.Items).To(Equal(items))
	})

	It("does not modify the items it is given", func() {
		items := []oscal.ControlItem{
			{ID: "grp", Class: oscal.ClassSection},
			{ID: "ctl", Text: "x", GroupID: "grp"},
		}
		_, err := oscal.BuildTree(items)
		Expect(err).NotTo(HaveOccurred())
		Expect(items[1].ContainerKey).To(BeEmpty())
	})

	DescribeTable("rejects items that cannot form a tree, naming the item and the fix",
		func(items []oscal.ControlItem, want string) {
			before := make([]oscal.ControlItem, len(items))
			copy(before, items)

			tree, err := oscal.BuildTree(items)

			Expect(tree).To(BeNil())
			Expect(err).To(MatchError(oscal.ErrInvalidItems))
			Expect(err).To(MatchError(ContainSubstring(want)))
			Expect(items).To(Equal(before))
		},
		Entry("a duplicate ID",
			[]oscal.ControlItem{{ID: "FA-01", Text: "a"}, {ID: "FA-01", Text: "b"}},
			`items 0 and 1 share the ID "FA-01"; every control, part and group needs its own ID`),
		Entry("a duplicate ID in another encoding",
			[]oscal.ControlItem{{ID: "07.03.01", Text: "a"}, {ID: "x", Text: "b"}, {ID: "07.03.01", Text: "c"}},
			`items 0 and 2 share the ID "07.03.01"`),
		Entry("an empty ID",
			[]oscal.ControlItem{{ID: "", Text: "a"}},
			"item 0 has no ID"),
		Entry("a group that names no item",
			[]oscal.ControlItem{{ID: "FA-01", Text: "a", GroupID: "missing"}},
			`item "FA-01" names group "missing", which is not an item`),
		Entry("a parent that names no item",
			[]oscal.ControlItem{{ID: "FA-01(02)", Text: "a", ParentID: "FA-01"}},
			`item "FA-01(02)" names parent "FA-01", which is not an item`),
		Entry("a parent that is a group",
			[]oscal.ControlItem{{ID: "grp", Class: oscal.ClassSection}, {ID: "a", GroupID: "grp"}, {ID: "b", ParentID: "grp"}},
			`item "b" names parent "grp", which is a group`),
		Entry("a group with a parent",
			[]oscal.ControlItem{{ID: "ctl", Text: "x"}, {ID: "grp", ParentID: "ctl"}, {ID: "a", GroupID: "grp"}},
			`group "grp" has parent "ctl"; a group nests only in another group (GroupID)`),
		Entry("a group cycle",
			[]oscal.ControlItem{{ID: "g1", GroupID: "g2"}, {ID: "g2", GroupID: "g1"}},
			`the GroupID chain of item "g1" loops back on itself`),
		Entry("a group naming itself",
			[]oscal.ControlItem{{ID: "g1", GroupID: "g1"}},
			`the GroupID chain of item "g1" loops back on itself`),
		Entry("a parent cycle",
			[]oscal.ControlItem{{ID: "a", ParentID: "b"}, {ID: "b", ParentID: "a"}},
			`the ParentID chain of item "a" loops back on itself`),
	)
})

var _ = Describe("Structurer tier reporting", func() {
	DescribeTable("StructureWithTier names the tier that produced the items",
		func(doc oscal.StructuredDoc, opts oscal.StructureOptions, completer oscal.Completer, want oscal.StructureTier) {
			var s oscal.Structurer
			if completer == nil {
				s = oscal.NewStructurer(nil, nil)
			} else {
				s = oscal.NewStructurer(completer, nil)
			}
			items, tier, err := s.StructureWithTier(context.Background(), doc, opts)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).NotTo(BeEmpty())
			Expect(tier).To(Equal(want))

			plain, err := s.Structure(context.Background(), doc, opts)
			Expect(err).NotTo(HaveOccurred())
			Expect(plain).To(Equal(items))
		},
		Entry("regex for a matching section pattern",
			oscal.StructuredDoc{RawText: "X-1: first\nX-2: second"},
			oscal.StructureOptions{SectionPattern: `^([A-Z]-\d+):\s*(.+)`}, nil, oscal.StructureTierRegex),
		Entry("heading for sections",
			oscal.StructuredDoc{Sections: []oscal.DocSection{{Level: 1, Title: "One", Text: "a"}}, RawText: "One\n\na"},
			oscal.StructureOptions{}, nil, oscal.StructureTierHeading),
		Entry("table for tables only",
			oscal.StructuredDoc{Tables: []oscal.DocTable{{Headers: []string{"ID", "Text"}, Rows: [][]string{{"R1", "do it"}}}}},
			oscal.StructureOptions{}, nil, oscal.StructureTierTable),
		Entry("pattern for numbered text",
			oscal.StructuredDoc{RawText: "1.1 One\n1.2 Two\n1.3 Three"},
			oscal.StructureOptions{}, nil, oscal.StructureTierPattern),
		Entry("llm-detect when a completer returns a usable pattern",
			oscal.StructuredDoc{RawText: "Rq one: first\nRq two: second"},
			oscal.StructureOptions{}, &testCompleter{response: `^Rq (\w+):\s*(.+)`}, oscal.StructureTierLLMDetect),
		Entry("llm-extract when a completer extracts items",
			oscal.StructuredDoc{RawText: "some prose without structure"},
			oscal.StructureOptions{}, &replyCompleter{replies: []string{"none", `[{"id":"r1","title":"T","text":"shall"}]`}}, oscal.StructureTierLLMExtract),
		Entry("fallback when nothing else matches and there is no completer",
			oscal.StructuredDoc{RawText: "plain prose.\n\nmore prose."},
			oscal.StructureOptions{}, nil, oscal.StructureTierFallback),
	)
})

// replyCompleter answers each Complete call with the next of replies; the
// first goes to the detect tier, the second to the extract tier.
type replyCompleter struct {
	replies []string
	calls   int
}

func (c *replyCompleter) Complete(_ context.Context, _ []oscal.Message) (string, error) {
	reply := c.replies[c.calls%len(c.replies)]
	c.calls++
	return reply, nil
}

// haveLinks matches a ControlItem's ID, ParentID, GroupID and ContainerKey.
func haveLinks(id, parentID, groupID, containerKey string) OmegaMatcher {
	return And(
		HaveField("ID", id),
		HaveField("ParentID", parentID),
		HaveField("GroupID", groupID),
		HaveField("ContainerKey", containerKey),
	)
}
