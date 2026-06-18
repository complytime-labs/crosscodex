package oscal_test

import (
	"context"
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"

	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

var _ = Describe("Assemble", func() {
	var assembler oscal.Assembler

	BeforeEach(func() {
		assembler = oscal.NewAssembler("")
	})

	Context("valid JSON output", func() {
		It("produces valid OSCAL JSON with correct metadata", func() {
			items := []oscal.ControlItem{
				{
					ID:      "ac-1",
					Title:   "Access Control Policy",
					Text:    "Develop and document access control policy.",
					Class:   oscal.ClassRequirement,
					GroupID: "ac",
				},
			}

			meta := oscal.CatalogMeta{
				Title:   "Test Catalog",
				Version: "1.0.0",
			}

			data, err := assembler.Assemble(context.Background(), items, meta)
			Expect(err).NotTo(HaveOccurred())

			var wrapper oscalTypes.OscalCompleteSchema
			Expect(json.Unmarshal(data, &wrapper)).To(Succeed())
			Expect(wrapper.Catalog).NotTo(BeNil())
			Expect(wrapper.Catalog.Metadata.Title).To(Equal("Test Catalog"))
			Expect(wrapper.Catalog.Metadata.Version).To(Equal("1.0.0"))
			Expect(wrapper.Catalog.Metadata.OscalVersion).To(Equal("1.1.3"))
			Expect(wrapper.Catalog.UUID).NotTo(BeEmpty())

			Expect(wrapper.Catalog.Groups).NotTo(BeNil())
			groups := *wrapper.Catalog.Groups
			Expect(groups).NotTo(BeEmpty())

			Expect(groups[0].Controls).NotTo(BeNil())
			controls := *groups[0].Controls
			Expect(controls).NotTo(BeEmpty())
			Expect(controls[0].ID).To(Equal("ac-1"))
			Expect(controls[0].Title).To(Equal("Access Control Policy"))
		})
	})

	Context("grouping by GroupID", func() {
		It("groups controls into separate OSCAL groups", func() {
			items := []oscal.ControlItem{
				{ID: "ac-1", Title: "Access Control Policy", Text: "Develop access control policy.", GroupID: "ac"},
				{ID: "ac-2", Title: "Account Management", Text: "Manage accounts.", GroupID: "ac"},
				{ID: "au-1", Title: "Audit Policy", Text: "Develop audit policy.", GroupID: "au"},
			}

			meta := oscal.CatalogMeta{Title: "Multi-Group Catalog", Version: "1.0.0"}

			data, err := assembler.Assemble(context.Background(), items, meta)
			Expect(err).NotTo(HaveOccurred())

			var wrapper oscalTypes.OscalCompleteSchema
			Expect(json.Unmarshal(data, &wrapper)).To(Succeed())
			Expect(wrapper.Catalog.Groups).NotTo(BeNil())

			groups := *wrapper.Catalog.Groups
			Expect(groups).To(HaveLen(2))

			groupIDs := make(map[string]int)
			for _, group := range groups {
				if group.Controls != nil {
					groupIDs[group.ID] = len(*group.Controls)
				}
			}
			Expect(groupIDs["ac"]).To(Equal(2))
			Expect(groupIDs["au"]).To(Equal(1))
		})
	})

	Context("parent-child wiring", func() {
		It("nests child controls as parts under parent controls", func() {
			items := []oscal.ControlItem{
				{ID: "ac-1", Title: "Access Control Policy", Text: "Parent control", GroupID: "ac"},
				{ID: "ac-1.a", Title: "Policy development", Text: "Child control A", ParentID: "ac-1", GroupID: "ac"},
				{ID: "ac-1.b", Title: "Policy dissemination", Text: "Child control B", ParentID: "ac-1", GroupID: "ac"},
				{ID: "ac-1.b.1", Title: "Dissemination methods", Text: "Grandchild control", ParentID: "ac-1.b", GroupID: "ac"},
			}

			meta := oscal.CatalogMeta{Title: "Parent-Child Catalog", Version: "1.0.0"}

			data, err := assembler.Assemble(context.Background(), items, meta)
			Expect(err).NotTo(HaveOccurred())

			var wrapper oscalTypes.OscalCompleteSchema
			Expect(json.Unmarshal(data, &wrapper)).To(Succeed())
			Expect(wrapper.Catalog.Groups).NotTo(BeNil())
			groups := *wrapper.Catalog.Groups
			Expect(groups).NotTo(BeEmpty())

			group := groups[0]
			Expect(group.Controls).NotTo(BeNil())
			controls := *group.Controls
			Expect(controls).NotTo(BeEmpty())

			ctrl := controls[0]
			Expect(ctrl.ID).To(Equal("ac-1"))
			Expect(ctrl.Parts).NotTo(BeNil())

			parts := *ctrl.Parts
			var childParts []oscalTypes.Part
			for _, part := range parts {
				if part.Name == "item" {
					childParts = append(childParts, part)
				}
			}
			Expect(childParts).To(HaveLen(2))

			var ac1b *oscalTypes.Part
			for i := range childParts {
				if childParts[i].ID == "ac-1.b" {
					ac1b = &childParts[i]
					break
				}
			}
			Expect(ac1b).NotTo(BeNil(), "expected to find ac-1.b part")
			Expect(ac1b.Parts).NotTo(BeNil())
			Expect(*ac1b.Parts).To(HaveLen(1))

			grandchild := (*ac1b.Parts)[0]
			Expect(grandchild.ID).To(Equal("ac-1.b.1"))
		})
	})

	Context("empty GroupID", func() {
		It("assigns controls to a default group", func() {
			items := []oscal.ControlItem{
				{ID: "misc-1", Title: "Miscellaneous Control", Text: "No group specified", GroupID: ""},
			}

			meta := oscal.CatalogMeta{Title: "Default Group Catalog", Version: "1.0.0"}

			data, err := assembler.Assemble(context.Background(), items, meta)
			Expect(err).NotTo(HaveOccurred())

			var wrapper oscalTypes.OscalCompleteSchema
			Expect(json.Unmarshal(data, &wrapper)).To(Succeed())
			Expect(wrapper.Catalog.Groups).NotTo(BeNil())
			groups := *wrapper.Catalog.Groups
			Expect(groups).NotTo(BeEmpty())
			Expect(groups[0].ID).To(Equal("default"))
		})
	})

	Context("well-formed JSON", func() {
		It("produces valid JSON that parses as both generic map and OSCAL schema", func() {
			items := []oscal.ControlItem{
				{
					ID:      "test-1",
					Title:   "Test Control",
					Text:    "Test prose with special chars: <>&\"'",
					GroupID: "test",
					Props: map[string]string{
						"key1": "value1",
						"key2": "value2",
					},
				},
			}

			meta := oscal.CatalogMeta{Title: "Test Catalog", Version: "1.0.0"}

			data, err := assembler.Assemble(context.Background(), items, meta)
			Expect(err).NotTo(HaveOccurred())

			var result map[string]interface{}
			Expect(json.Unmarshal(data, &result)).To(Succeed())
			Expect(result).To(HaveKey("catalog"))

			var wrapper oscalTypes.OscalCompleteSchema
			Expect(json.Unmarshal(data, &wrapper)).To(Succeed())
			Expect(wrapper.Catalog).NotTo(BeNil())
		})
	})
})

var _ = Describe("Structurer", func() {
	var structurer oscal.Structurer

	BeforeEach(func() {
		structurer = oscal.NewStructurer(nil, nil)
	})

	Context("when falling through tiers to find a match", func() {
		It("uses TierHeading for a document with sections", func() {
			doc := oscal.StructuredDoc{
				Sections: []oscal.DocSection{
					{Level: 1, Title: "Introduction", Text: "This is the introduction."},
					{Level: 1, Title: "Requirements", Text: "The system shall implement authentication."},
				},
				RawText: "Introduction\n\nThis is the introduction.\n\nRequirements\n\nThe system shall implement authentication.",
			}

			items, err := structurer.Structure(context.Background(), doc, oscal.StructureOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(2))
			Expect(items[0].Title).To(Equal("Introduction"))
			Expect(items[1].Title).To(Equal("Requirements"))
		})
	})

	Context("when falling to paragraph fallback", func() {
		It("splits plain text into paragraphs", func() {
			doc := oscal.StructuredDoc{
				RawText: "This is the first paragraph.\n\nThis is the second paragraph.\n\nThis is the third paragraph.",
			}

			items, err := structurer.Structure(context.Background(), doc, oscal.StructureOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(3))
			Expect(items[0].ID).To(Equal("para-1"))
			Expect(items[1].ID).To(Equal("para-2"))
			Expect(items[2].ID).To(Equal("para-3"))
		})
	})

	Context("when applying keyword filtering", func() {
		It("keeps only items matching the keywords", func() {
			doc := oscal.StructuredDoc{
				Sections: []oscal.DocSection{
					{Level: 1, Title: "Background", Text: "This is background information."},
					{Level: 1, Title: "Requirements", Text: "The system shall implement authentication."},
					{Level: 1, Title: "Overview", Text: "This is an overview."},
					{Level: 1, Title: "Compliance", Text: "All systems must comply with these requirements."},
				},
				RawText: "Background\n\nThis is background information.\n\nRequirements\n\nThe system shall implement authentication.\n\nOverview\n\nThis is an overview.\n\nCompliance\n\nAll systems must comply with these requirements.",
			}

			opts := oscal.StructureOptions{
				FilterByKeywords: true,
				Keywords:         []string{"shall", "must"},
			}

			items, err := structurer.Structure(context.Background(), doc, opts)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(2))

			for _, item := range items {
				Expect(item.Title).To(BeElementOf("Requirements", "Compliance"))
			}
		})

		It("keeps the original set when all items are filtered out", func() {
			doc := oscal.StructuredDoc{
				Sections: []oscal.DocSection{
					{Level: 1, Title: "Background", Text: "This is background information."},
					{Level: 1, Title: "Overview", Text: "This is an overview."},
				},
				RawText: "Background\n\nThis is background information.\n\nOverview\n\nThis is an overview.",
			}

			opts := oscal.StructureOptions{
				FilterByKeywords: true,
				Keywords:         []string{"shall", "must"},
			}

			items, err := structurer.Structure(context.Background(), doc, opts)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(2))
		})
	})

	Context("when applying decomposition", func() {
		It("decomposes parenthesized clauses into sub-items", func() {
			doc := oscal.StructuredDoc{
				Sections: []oscal.DocSection{
					{
						Level: 1,
						Title: "Access Control",
						Text:  "The system shall:\n(a) implement authentication mechanisms\n(b) implement authorization controls\n(c) implement audit logging",
					},
				},
				RawText: "Access Control\n\nThe system shall:\n(a) implement authentication mechanisms\n(b) implement authorization controls\n(c) implement audit logging",
			}

			opts := oscal.StructureOptions{
				Decompose:         true,
				MinDecomposeWords: 5,
			}

			items, err := structurer.Structure(context.Background(), doc, opts)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(items)).To(BeNumerically(">=", 3))

			ids := make(map[string]bool)
			for _, item := range items {
				ids[item.ID] = true
			}
			Expect(ids).To(HaveKey("sec-1.a"))
			Expect(ids).To(HaveKey("sec-1.b"))
			Expect(ids).To(HaveKey("sec-1.c"))
		})

		It("respects MinDecomposeWords threshold", func() {
			doc := oscal.StructuredDoc{
				Sections: []oscal.DocSection{
					{Level: 1, Title: "Short", Text: "(a) short clause"},
				},
				RawText: "Short\n\n(a) short clause",
			}

			opts := oscal.StructureOptions{
				Decompose:         true,
				MinDecomposeWords: 40,
			}

			items, err := structurer.Structure(context.Background(), doc, opts)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(1))
			Expect(items[0].ID).To(Equal("sec-1"))
		})
	})

	Context("when combining filtering and decomposition", func() {
		It("filters first then decomposes matching items", func() {
			doc := oscal.StructuredDoc{
				Sections: []oscal.DocSection{
					{
						Level: 1,
						Title: "Requirements",
						Text:  "The system shall:\n(a) implement authentication mechanisms\n(b) implement authorization controls",
					},
					{
						Level: 1,
						Title: "Background",
						Text:  "This is background information that does not contain requirements keywords.",
					},
				},
				RawText: "Requirements\n\nThe system shall:\n(a) implement authentication mechanisms\n(b) implement authorization controls\n\nBackground\n\nThis is background information that does not contain requirements keywords.",
			}

			opts := oscal.StructureOptions{
				FilterByKeywords:  true,
				Keywords:          []string{"shall"},
				Decompose:         true,
				MinDecomposeWords: 5,
			}

			items, err := structurer.Structure(context.Background(), doc, opts)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(items)).To(BeNumerically(">=", 2))

			for _, item := range items {
				Expect(item.ID).To(BeElementOf("sec-1", "sec-1.a", "sec-1.b"))
			}
		})
	})

	Context("when all tiers fail", func() {
		It("returns ErrStructureFailed for empty document", func() {
			doc := oscal.StructuredDoc{RawText: ""}

			_, err := structurer.Structure(context.Background(), doc, oscal.StructureOptions{})
			Expect(err).To(MatchError(oscal.ErrStructureFailed))
		})
	})

	Context("when using a custom regex pattern", func() {
		It("extracts items matching the regex from raw text", func() {
			doc := oscal.StructuredDoc{
				Sections: []oscal.DocSection{
					{Level: 1, Title: "Section 1", Text: "Text 1"},
				},
				RawText: "REQ-001: First requirement\nREQ-002: Second requirement\nREQ-003: Third requirement",
			}

			opts := oscal.StructureOptions{
				SectionPattern: `^(REQ-\d+):\s+(.+)$`,
			}

			items, err := structurer.Structure(context.Background(), doc, opts)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(3))
			Expect(items[0].ID).To(Equal("REQ-001"))
		})
	})
})

var _ = Describe("TierFallback", func() {
	DescribeTable("paragraph splitting",
		func(rawText string, expectedLen int, expectedIDs []string, expectedTexts []string) {
			doc := oscal.StructuredDoc{RawText: rawText}
			opts := oscal.StructureOptions{}

			items, ok := oscal.TierFallback(doc, opts)
			Expect(ok).To(BeTrue(), "fallback should always succeed")
			Expect(items).To(HaveLen(expectedLen))

			for i, expID := range expectedIDs {
				Expect(items[i].ID).To(Equal(expID))
			}
			for i, expText := range expectedTexts {
				Expect(items[i].Text).To(Equal(expText))
			}
			for _, item := range items {
				Expect(item.Class).To(Equal(oscal.ClassRequirement))
			}
		},
		Entry("splits by double newline",
			"First paragraph with some text.\n\nSecond paragraph with more content.\n\nThird paragraph here.",
			3,
			[]string{"para-1", "para-2", "para-3"},
			[]string{"First paragraph with some text.", "Second paragraph with more content.", "Third paragraph here."},
		),
		Entry("skips empty paragraphs",
			"First paragraph.\n\n\nSecond paragraph.\n\n",
			2,
			[]string{"para-1", "para-2"},
			[]string{"First paragraph.", "Second paragraph."},
		),
		Entry("handles single paragraph",
			"Just one paragraph with no breaks.",
			1,
			[]string{"para-1"},
			[]string{"Just one paragraph with no breaks."},
		),
		Entry("trims whitespace from paragraphs",
			"  First with leading/trailing spaces\n\n  Second paragraph  ",
			2,
			[]string{"para-1", "para-2"},
			[]string{"First with leading/trailing spaces", "Second paragraph"},
		),
		Entry("handles multiple consecutive empty paragraphs",
			"Content\n\n\n\nAnother content",
			2,
			[]string{"para-1", "para-3"},
			[]string{"Content", "Another content"},
		),
	)

	It("returns true with zero items for empty text", func() {
		doc := oscal.StructuredDoc{RawText: ""}
		items, ok := oscal.TierFallback(doc, oscal.StructureOptions{})
		Expect(ok).To(BeTrue(), "fallback always succeeds")
		Expect(items).To(BeEmpty())
	})

	It("returns true with zero items for whitespace-only text", func() {
		doc := oscal.StructuredDoc{RawText: "\n\n\n   \n\n"}
		items, ok := oscal.TierFallback(doc, oscal.StructureOptions{})
		Expect(ok).To(BeTrue(), "fallback always succeeds")
		Expect(items).To(BeEmpty())
	})
})

var _ = Describe("TierHeading", func() {
	Context("when no sections are present", func() {
		It("returns false with nil items", func() {
			doc := oscal.StructuredDoc{Sections: []oscal.DocSection{}}
			items, ok := oscal.TierHeading(doc, oscal.StructureOptions{})
			Expect(ok).To(BeFalse())
			Expect(items).To(BeNil())
		})
	})

	Context("when sections have unique titles", func() {
		It("extracts all sections as control items", func() {
			doc := oscal.StructuredDoc{
				Sections: []oscal.DocSection{
					{Level: 1, Title: "Introduction", Text: "This is the intro"},
					{Level: 2, Title: "Requirements", Text: "These are requirements"},
					{Level: 2, Title: "Compliance", Text: "Compliance rules"},
				},
			}

			items, ok := oscal.TierHeading(doc, oscal.StructureOptions{})
			Expect(ok).To(BeTrue())
			Expect(items).To(HaveLen(3))

			expected := []struct {
				id, title, text string
			}{
				{"sec-1", "Introduction", "This is the intro"},
				{"sec-2", "Requirements", "These are requirements"},
				{"sec-3", "Compliance", "Compliance rules"},
			}

			for i, exp := range expected {
				Expect(items[i].ID).To(Equal(exp.id))
				Expect(items[i].Title).To(Equal(exp.title))
				Expect(items[i].Text).To(Equal(exp.text))
				Expect(items[i].Class).To(Equal(oscal.ClassRequirement))
			}
		})
	})

	Context("when filtering repeated headings", func() {
		It("filters headings exceeding default MaxHeadingRepeats of 3", func() {
			doc := oscal.StructuredDoc{
				Sections: []oscal.DocSection{
					{Level: 1, Title: "Spurious", Text: "First"},
					{Level: 1, Title: "Spurious", Text: "Second"},
					{Level: 1, Title: "Spurious", Text: "Third"},
					{Level: 1, Title: "Spurious", Text: "Fourth"},
					{Level: 1, Title: "Real Heading", Text: "Real content"},
				},
			}

			items, ok := oscal.TierHeading(doc, oscal.StructureOptions{})
			Expect(ok).To(BeTrue())
			Expect(items).To(HaveLen(1))
			Expect(items[0].Title).To(Equal("Real Heading"))
		})

		It("respects custom MaxHeadingRepeats", func() {
			doc := oscal.StructuredDoc{
				Sections: []oscal.DocSection{
					{Level: 1, Title: "Common", Text: "First"},
					{Level: 1, Title: "Common", Text: "Second"},
					{Level: 1, Title: "Unique", Text: "Content"},
				},
			}

			opts := oscal.StructureOptions{MaxHeadingRepeats: 1}
			items, ok := oscal.TierHeading(doc, opts)
			Expect(ok).To(BeTrue())
			Expect(items).To(HaveLen(1))
			Expect(items[0].Title).To(Equal("Unique"))
		})

		It("returns false when all sections are filtered out", func() {
			doc := oscal.StructuredDoc{
				Sections: []oscal.DocSection{
					{Level: 1, Title: "Same", Text: "A"},
					{Level: 1, Title: "Same", Text: "B"},
					{Level: 1, Title: "Same", Text: "C"},
					{Level: 1, Title: "Same", Text: "D"},
				},
			}

			opts := oscal.StructureOptions{MaxHeadingRepeats: 3}
			items, ok := oscal.TierHeading(doc, opts)
			Expect(ok).To(BeFalse())
			Expect(items).To(BeNil())
		})
	})
})
