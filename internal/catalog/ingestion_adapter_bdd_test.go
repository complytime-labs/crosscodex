//go:build !integration

package catalog_test

import (
	"encoding/json"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

// ingestionFixtureElement is the subset of a crosscodex-ingestion element the
// specs compare against; it is decoded independently of the adapter.
type ingestionFixtureElement struct {
	ElementID string  `json:"element_id"`
	Type      string  `json:"type"`
	Content   string  `json:"content"`
	Level     *int    `json:"level"`
	ParentID  *string `json:"parent_id"`
}

func readIngestionFixture(name string) ([]byte, []ingestionFixtureElement) {
	data, err := os.ReadFile(filepath.Join("testdata", "ingestion", name))
	Expect(err).NotTo(HaveOccurred())
	var out struct {
		Elements []ingestionFixtureElement `json:"elements"`
	}
	Expect(json.Unmarshal(data, &out)).To(Succeed())
	return data, out.Elements
}

// ingestionDoc builds a schema-v1 document from elements given as maps, so a
// spec states only the fields under test.
func ingestionDoc(version string, elements ...map[string]any) []byte {
	for i, e := range elements {
		defaults := map[string]any{
			"element_id": "e-" + string(rune('a'+i)),
			"type":       "paragraph",
			"content":    "text",
			"page":       nil,
			"level":      nil,
			"parent_id":  nil,
			"style":      map[string]any{},
		}
		for k, v := range e {
			defaults[k] = v
		}
		elements[i] = defaults
	}
	if elements == nil {
		elements = []map[string]any{}
	}
	data, err := json.Marshal(map[string]any{
		"schema_version": version,
		"elements":       elements,
		"metadata":       map[string]any{"element_count": len(elements), "dropped": []any{}},
	})
	Expect(err).NotTo(HaveOccurred())
	return data
}

func heading(id string, level int, parent any, content string) map[string]any {
	return map[string]any{"element_id": id, "type": "heading", "level": level, "parent_id": parent, "content": content}
}

// withRole makes el a schema 1.3 source node: it sets id, role, title and text
// (nil encodes null) and, when props is non-nil, properties.
func withRole(el map[string]any, id, role string, title, text any, props map[string]string) map[string]any {
	el["id"], el["role"], el["title"], el["text"] = id, role, title, text
	if props != nil {
		el["properties"] = props
	}
	return el
}

// withCatalogMeta sets metadata.title and metadata.source.catalog_version on
// a document built by ingestionDoc (nil encodes null).
func withCatalogMeta(data []byte, title, version any) []byte {
	var doc map[string]any
	Expect(json.Unmarshal(data, &doc)).To(Succeed())
	meta := doc["metadata"].(map[string]any)
	meta["title"] = title
	meta["source"] = map[string]any{"kind": "structured", "format": "json", "catalog_version": version}
	out, err := json.Marshal(doc)
	Expect(err).NotTo(HaveOccurred())
	return out
}

var _ = Describe("AdaptIngestionOutput", func() {
	DescribeTable("adapts real converter output",
		func(fixture string) {
			data, elements := readIngestionFixture(fixture)

			doc, meta, err := catalog.AdaptIngestionOutput(data)
			Expect(err).NotTo(HaveOccurred())
			Expect(meta.SchemaVersion).To(MatchRegexp(`^1\.\d+$`))

			var wantTitles []string
			var wantLevels []int
			textUnder := map[string][]string{}
			tableCount := 0
			for _, e := range elements {
				switch e.Type {
				case "heading":
					wantTitles = append(wantTitles, e.Content)
					wantLevels = append(wantLevels, *e.Level)
				case "table":
					tableCount++
				}
				if e.Type != "heading" && e.ParentID != nil {
					textUnder[*e.ParentID] = append(textUnder[*e.ParentID], e.Content)
				}
				Expect(doc.RawText).To(ContainSubstring(e.Content))
			}

			var gotTitles []string
			var gotLevels []int
			for _, s := range doc.Sections {
				gotTitles = append(gotTitles, s.Title)
				gotLevels = append(gotLevels, s.Level)
			}
			Expect(gotTitles).To(Equal(wantTitles))
			Expect(gotLevels).To(Equal(wantLevels))
			Expect(gotLevels).To(ContainElement(BeNumerically(">", 1)))

			headingIndex := 0
			for _, e := range elements {
				if e.Type != "heading" {
					continue
				}
				for _, body := range textUnder[e.ElementID] {
					Expect(doc.Sections[headingIndex].Text).To(ContainSubstring(body))
				}
				headingIndex++
			}

			Expect(doc.Tables).To(HaveLen(tableCount))
			Expect(tableCount).To(BeNumerically(">", 0))
			for _, t := range doc.Tables {
				Expect(t.Headers).NotTo(BeEmpty())
				Expect(t.Rows).NotTo(BeEmpty())
				for _, row := range t.Rows {
					Expect(row).To(HaveLen(len(t.Headers)))
				}
			}
		},
		Entry("NIST SP 800-171r3 PDF excerpt", "nist-sp800-171r3-excerpt.json"),
		Entry("EUR-Lex NIS2 HTML excerpt", "eurlex-nis2-32022L2555.json"),
	)

	It("ignores fields it does not know, at the top level and on elements", func() {
		plain := ingestionDoc("1.2", heading("e-1", 1, nil, "Scope"), map[string]any{"parent_id": "e-1"})
		var withExtra map[string]any
		Expect(json.Unmarshal(plain, &withExtra)).To(Succeed())
		withExtra["future_field"] = map[string]any{"x": 1}
		withExtra["elements"].([]any)[0].(map[string]any)["future_element_field"] = []any{"y"}
		extra, err := json.Marshal(withExtra)
		Expect(err).NotTo(HaveOccurred())

		want, _, err := catalog.AdaptIngestionOutput(plain)
		Expect(err).NotTo(HaveOccurred())
		got, _, err := catalog.AdaptIngestionOutput(extra)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(want))
	})

	It("accepts a later minor of the supported major and reports the version", func() {
		_, meta, err := catalog.AdaptIngestionOutput(ingestionDoc("1.9", heading("e-1", 1, nil, "Scope")))
		Expect(err).NotTo(HaveOccurred())
		Expect(meta.SchemaVersion).To(Equal("1.9"))
	})

	// Schema keys are case-sensitive: a wrong-case key is not the schema key,
	// so it is an unknown field and is ignored, never folded onto the real one.
	It("ignores wrong-case element keys as unknown fields instead of folding them", func() {
		doc, _, err := catalog.AdaptIngestionOutput(ingestionDoc("1.0",
			map[string]any{"element_id": "p", "Type": "figure", "Level": 2, "CONTENT": " ", "Parent_ID": "x"}))
		Expect(err).NotTo(HaveOccurred())
		Expect(doc.Sections).To(BeEmpty())
		Expect(doc.RawText).To(Equal("text"))
	})

	It("puts a heading's child text in its section and every element in RawText, in order", func() {
		data := ingestionDoc("1.0",
			map[string]any{"element_id": "p0", "content": "Preamble."},
			heading("h1", 1, nil, "Part One"),
			map[string]any{"element_id": "p1", "content": "First.", "parent_id": "h1"},
			heading("h2", 2, "h1", "Part One A"),
			map[string]any{"element_id": "l1", "type": "list_item", "content": "Item.", "parent_id": "h2"},
			map[string]any{"element_id": "p2", "content": "Second.", "parent_id": "h1"},
		)
		doc, _, err := catalog.AdaptIngestionOutput(data)
		Expect(err).NotTo(HaveOccurred())
		Expect(doc.Sections).To(HaveLen(2))
		Expect(doc.Sections[0].Title).To(Equal("Part One"))
		Expect(doc.Sections[0].Level).To(Equal(1))
		Expect(doc.Sections[0].Text).To(Equal("First.\n\nSecond."))
		Expect(doc.Sections[1].Level).To(Equal(2))
		Expect(doc.Sections[1].Text).To(Equal("Item."))
		Expect(doc.RawText).To(Equal("Preamble.\n\nPart One\n\nFirst.\n\nPart One A\n\nItem.\n\nSecond."))
	})

	It("parses a Markdown table after its caption, unescaping pipes and trimming cells", func() {
		table := "Table 9. Caption\n\n| A | B \\| C |\n|:---|---:|\n| 1 | x \\| y |\n|  | 2 |"
		doc, _, err := catalog.AdaptIngestionOutput(ingestionDoc("1.0",
			map[string]any{"type": "table", "content": table}))
		Expect(err).NotTo(HaveOccurred())
		Expect(doc.Tables).To(HaveLen(1))
		Expect(doc.Tables[0].Headers).To(Equal([]string{"A", "B | C"}))
		Expect(doc.Tables[0].Rows).To(Equal([][]string{{"1", "x | y"}, {"", "2"}}))
		Expect(doc.RawText).To(Equal(table))
	})

	DescribeTable("rejects output it cannot trust, with an actionable error",
		func(data []byte, sentinel error, wantMessage string) {
			var err error
			Expect(func() { _, _, err = catalog.AdaptIngestionOutput(data) }).NotTo(Panic())
			Expect(err).To(MatchError(sentinel))
			Expect(err).To(MatchError(ContainSubstring(wantMessage)))
		},
		Entry("an unsupported major names the supported one",
			ingestionDoc("2.0", heading("e-1", 1, nil, "Scope")),
			catalog.ErrUnsupportedSchema, `schema_version "2.0" has major 2, but this server reads major 1`),
		Entry("an unsupported major says to re-run conversion",
			ingestionDoc("2.0", heading("e-1", 1, nil, "Scope")),
			catalog.ErrUnsupportedSchema, "re-run conversion with a matching ingestion version"),
		Entry("a schema_version that is not MAJOR.MINOR",
			ingestionDoc("v1", heading("e-1", 1, nil, "Scope")),
			catalog.ErrUnsupportedSchema, "is not MAJOR.MINOR"),
		Entry("a missing schema_version",
			[]byte(`{"elements":[]}`),
			catalog.ErrUnsupportedSchema, "is not MAJOR.MINOR"),
		Entry("empty elements",
			ingestionDoc("1.2"),
			catalog.ErrInvalidIngestionOutput, "has no elements"),
		Entry("malformed JSON",
			[]byte(`{"schema_version":"1.0","elements":[`),
			catalog.ErrInvalidIngestionOutput, "is not valid JSON"),
		Entry("a JSON array instead of an object",
			[]byte(`[1,2]`),
			catalog.ErrInvalidIngestionOutput, "is not valid JSON"),
		Entry("trailing data after the document",
			append(ingestionDoc("1.0", heading("e-1", 1, nil, "Scope")), []byte(`{}`)...),
			catalog.ErrInvalidIngestionOutput, "is not valid JSON"),
		Entry("a fractional level",
			[]byte(`{"schema_version":"1.0","elements":[{"element_id":"e","type":"heading","content":"x","level":1.5}]}`),
			catalog.ErrInvalidIngestionOutput, "is not valid JSON"),
		Entry("an unknown element type",
			ingestionDoc("1.2", map[string]any{"type": "figure"}),
			catalog.ErrInvalidIngestionOutput, `elements[0] has type "figure"`),
		Entry("blank content",
			ingestionDoc("1.2", map[string]any{"content": " \n\t"}),
			catalog.ErrInvalidIngestionOutput, "elements[0] has blank content"),
		Entry("a missing element_id",
			ingestionDoc("1.2", map[string]any{"element_id": ""}),
			catalog.ErrInvalidIngestionOutput, "elements[0] has no element_id"),
		Entry("a duplicate element_id",
			ingestionDoc("1.2", map[string]any{"element_id": "x"}, map[string]any{"element_id": "x"}),
			catalog.ErrInvalidIngestionOutput, "elements[1] repeats the element_id of an earlier element"),
		Entry("a heading without a level",
			ingestionDoc("1.2", map[string]any{"type": "heading"}),
			catalog.ErrInvalidIngestionOutput, "elements[0] is a heading with level null"),
		Entry("a heading level below 1",
			ingestionDoc("1.2", heading("e-1", 0, nil, "Scope")),
			catalog.ErrInvalidIngestionOutput, "elements[0] is a heading with level 0, outside 1 to 100"),
		Entry("a heading level above 100",
			ingestionDoc("1.2", heading("e-1", 101, nil, "Scope")),
			catalog.ErrInvalidIngestionOutput, "elements[0] is a heading with level 101, outside 1 to 100"),
		Entry("a level on a non-heading",
			ingestionDoc("1.2", map[string]any{"level": 2}),
			catalog.ErrInvalidIngestionOutput, "elements[0] is a paragraph with level 2"),
		Entry("a parent_id that refers forward",
			ingestionDoc("1.2", map[string]any{"parent_id": "h"}, heading("h", 1, nil, "Scope")),
			catalog.ErrInvalidIngestionOutput, "elements[0] has a parent_id that is not an earlier heading"),
		Entry("a parent_id that refers to a non-heading",
			ingestionDoc("1.2", map[string]any{"element_id": "p"}, map[string]any{"element_id": "q", "parent_id": "p"}),
			catalog.ErrInvalidIngestionOutput, "elements[1] has a parent_id that is not an earlier heading"),
		Entry("a table without a header separator row",
			ingestionDoc("1.2", map[string]any{"type": "table", "content": "| a | b |\n| 1 | 2 |"}),
			catalog.ErrInvalidIngestionOutput, "elements[0] is a table with no Markdown header row"),
		Entry("a wrong-case SCHEMA_VERSION, which is unknown, so schema_version is missing",
			[]byte(`{"SCHEMA_VERSION":"1.0","elements":[{"element_id":"e","type":"paragraph","content":"x"}]}`),
			catalog.ErrUnsupportedSchema, "schema_version is not MAJOR.MINOR (it is missing)"),
		Entry("a wrong-case Elements, which is unknown, so elements is empty",
			[]byte(`{"schema_version":"1.0","Elements":[{"element_id":"e","type":"paragraph","content":"x"}]}`),
			catalog.ErrInvalidIngestionOutput, "has no elements"),
		Entry("a wrong-case Level on a heading, which leaves level null",
			[]byte(`{"schema_version":"1.0","elements":[{"element_id":"e","type":"heading","content":"x","Level":1}]}`),
			catalog.ErrInvalidIngestionOutput, "elements[0] is a heading with level null"),
		Entry("a wrong-case Element_ID, which leaves element_id missing",
			[]byte(`{"schema_version":"1.0","elements":[{"Element_ID":"e","type":"paragraph","content":"x"}]}`),
			catalog.ErrInvalidIngestionOutput, "elements[0] has no element_id"),
		Entry("an element field of the wrong JSON type",
			[]byte(`{"schema_version":"1.0","elements":[{"element_id":7,"type":"paragraph","content":"x"}]}`),
			catalog.ErrInvalidIngestionOutput, "elements[0].element_id is not valid JSON"),
		// Source-node and metadata keys are decoded on every path, so a
		// schema-invalid value fails output without roles too.
		Entry("a non-string id on output without roles",
			ingestionDoc("1.2", map[string]any{"id": 5}),
			catalog.ErrInvalidIngestionOutput, "elements[0].id is not valid JSON"),
		Entry("metadata that is not an object, on output without roles",
			[]byte(`{"schema_version":"1.2","metadata":"x","elements":[{"element_id":"e","type":"paragraph","content":"x"}]}`),
			catalog.ErrInvalidIngestionOutput, "metadata is not valid JSON"),
		Entry("a table whose delimiter row has a different number of cells",
			ingestionDoc("1.2", map[string]any{"type": "table", "content": "| a | b |\n|---|\n| 1 | 2 |"}),
			catalog.ErrInvalidIngestionOutput, "elements[0] is a table whose delimiter row has 1 cells, but its header has 2"),
		Entry("a table row with a different number of cells",
			ingestionDoc("1.2", map[string]any{"type": "table", "content": "| a | b |\n|---|---|\n| 1 |"}),
			catalog.ErrInvalidIngestionOutput, "elements[0] is a table whose row 1 has 1 cells, but its header has 2"),
	)
})

// sensitiveControl is a top-level role control paragraph whose id, title and
// text carry markers a contract-break spec asserts never reach the error.
func sensitiveControl(elementID, id string) map[string]any {
	return withRole(map[string]any{"element_id": elementID, "content": "Sensitive body."},
		id, "control", "Sensitive title", "Sensitive body.", nil)
}

func without(el map[string]any, key string) map[string]any {
	delete(el, key)
	return el
}

var _ = Describe("AdaptIngestionOutput with element roles", func() {
	It("maps an ASVS-shaped output to source items, skipping id-less text", func() {
		data := withCatalogMeta(ingestionDoc("1.3",
			withRole(heading("e-1", 1, nil, "Encoding and Sanitization"), "V1", "group", "Encoding and Sanitization", "Chapter intro.", nil),
			map[string]any{"element_id": "e-2", "content": "Chapter intro.", "parent_id": "e-1"},
			withRole(heading("e-3", 2, "e-1", "Architecture"), "V1.1", "group", "Architecture", nil, nil),
			withRole(map[string]any{"element_id": "e-4", "content": "Verify one.", "parent_id": "e-3"},
				"V1.1.1", "control", nil, "Verify one.", map[string]string{"level": "1"}),
			withRole(map[string]any{"element_id": "e-5", "content": "Verify two.", "parent_id": "e-3"},
				"V1.1.2", "control", nil, "Verify two.", map[string]string{"level": "2"}),
		), "Test Verification Standard", "5.0.0")

		doc, meta, err := catalog.AdaptIngestionOutput(data)
		Expect(err).NotTo(HaveOccurred())
		Expect(doc).To(BeZero())
		Expect(meta.SchemaVersion).To(Equal("1.3"))
		Expect(meta.CatalogTitle).To(Equal("Test Verification Standard"))
		Expect(meta.CatalogVersion).To(Equal("5.0.0"))
		Expect(meta.SourceItems).To(Equal([]oscal.ControlItem{
			{ID: "V1", Title: "Encoding and Sanitization", Text: "Chapter intro."},
			{ID: "V1.1", Title: "Architecture", GroupID: "V1"},
			{ID: "V1.1.1", Text: "Verify one.", GroupID: "V1.1", Class: oscal.ClassRequirement, Props: map[string]string{"level": "1"}},
			{ID: "V1.1.2", Text: "Verify two.", GroupID: "V1.1", Class: oscal.ClassRequirement, Props: map[string]string{"level": "2"}},
		}))
	})

	It("links a control under a control by ParentID, outside any group", func() {
		data := withCatalogMeta(ingestionDoc("1.3",
			withRole(heading("e-1", 1, nil, "Account Management"), "AC-2", "control", "Account Management", "Manage accounts.", nil),
			map[string]any{"element_id": "e-2", "content": "Manage accounts.", "parent_id": "e-1", "id": nil},
			withRole(map[string]any{"element_id": "e-3", "content": "Automate it.", "parent_id": "e-1"},
				"AC-2(1)", "control", "Automated Account Management", "Automate it.", nil),
		), "Test Control Catalog", nil)

		_, meta, err := catalog.AdaptIngestionOutput(data)
		Expect(err).NotTo(HaveOccurred())
		Expect(meta.CatalogTitle).To(Equal("Test Control Catalog"))
		Expect(meta.CatalogVersion).To(BeEmpty())
		Expect(meta.SourceItems).To(Equal([]oscal.ControlItem{
			{ID: "AC-2", Title: "Account Management", Text: "Manage accounts.", Class: oscal.ClassRequirement},
			{ID: "AC-2(1)", Title: "Automated Account Management", Text: "Automate it.", ParentID: "AC-2", Class: oscal.ClassRequirement},
		}))
	})

	It("decodes null title, text and catalog title to empty strings", func() {
		data := withCatalogMeta(ingestionDoc("1.3",
			withRole(map[string]any{"element_id": "e-1", "content": "Body."}, "C-1", "control", nil, nil, nil),
		), nil, nil)

		_, meta, err := catalog.AdaptIngestionOutput(data)
		Expect(err).NotTo(HaveOccurred())
		Expect(meta.CatalogTitle).To(BeEmpty())
		Expect(meta.SourceItems).To(Equal([]oscal.ControlItem{{ID: "C-1", Class: oscal.ClassRequirement}}))
	})

	DescribeTable("rejects output breaking the role contract",
		func(data []byte, wantIndex string) {
			before := append([]byte(nil), data...)
			var (
				doc  oscal.StructuredDoc
				meta catalog.IngestionMeta
				err  error
			)
			Expect(func() { doc, meta, err = catalog.AdaptIngestionOutput(data) }).NotTo(Panic())
			Expect(err).To(MatchError(catalog.ErrInvalidIngestionOutput))
			Expect(err).To(MatchError(ContainSubstring(wantIndex)))
			Expect(err).To(MatchError(ContainSubstring("Re-run the conversion")))
			Expect(err.Error()).NotTo(ContainSubstring("Sensitive"))
			Expect(err.Error()).NotTo(ContainSubstring("SID-"))
			Expect(doc).To(BeZero())
			Expect(meta).To(BeZero())
			Expect(data).To(Equal(before))
		},
		Entry("an id without a role",
			ingestionDoc("1.3", sensitiveControl("e-1", "SID-1"), without(sensitiveControl("e-2", "SID-2"), "role")),
			"elements[1]"),
		Entry("a role without an id",
			ingestionDoc("1.3", sensitiveControl("e-1", "SID-1"), without(sensitiveControl("e-2", "SID-2"), "id")),
			"elements[1]"),
		Entry("a role with a null id",
			ingestionDoc("1.3", sensitiveControl("e-1", "SID-1"),
				map[string]any{"element_id": "e-2", "content": "Sensitive body.", "id": nil, "role": "control", "title": nil, "text": nil}),
			"elements[1]"),
		Entry("a blank id",
			ingestionDoc("1.3", sensitiveControl("e-1", "SID-1"), sensitiveControl("e-2", " ")),
			"elements[1]"),
		Entry("a role outside group and control",
			ingestionDoc("1.3", sensitiveControl("e-1", "SID-1"),
				withRole(map[string]any{"element_id": "e-2", "content": "Sensitive body."}, "SID-2", "part", nil, nil, nil)),
			"elements[1]"),
		Entry("an id and role without the title key",
			ingestionDoc("1.3", sensitiveControl("e-1", "SID-1"), without(sensitiveControl("e-2", "SID-2"), "title")),
			"elements[1]"),
		Entry("an id and role without the text key",
			ingestionDoc("1.3", sensitiveControl("e-1", "SID-1"), without(sensitiveControl("e-2", "SID-2"), "text")),
			"elements[1]"),
		Entry("a group whose parent is a control",
			ingestionDoc("1.3",
				withRole(heading("e-1", 1, nil, "Sensitive heading"), "SID-1", "control", "Sensitive title", nil, nil),
				withRole(heading("e-2", 2, "e-1", "Sensitive heading"), "SID-2", "group", "Sensitive title", nil, nil),
				withRole(map[string]any{"element_id": "e-3", "content": "Sensitive body.", "parent_id": "e-2"}, "SID-3", "control", nil, "Sensitive body.", nil)),
			"elements[1]"),
		Entry("a group without children",
			ingestionDoc("1.3",
				sensitiveControl("e-1", "SID-1"),
				withRole(heading("e-2", 1, nil, "Sensitive heading"), "SID-2", "group", "Sensitive title", nil, nil)),
			"elements[1]"),
		Entry("a group with a null title",
			ingestionDoc("1.3",
				withRole(heading("e-1", 1, nil, "Sensitive heading"), "SID-1", "group", nil, nil, nil),
				withRole(map[string]any{"element_id": "e-2", "content": "Sensitive body.", "parent_id": "e-1"}, "SID-2", "control", nil, "Sensitive body.", nil)),
			"elements[0]"),
		Entry("text without an id",
			ingestionDoc("1.3", sensitiveControl("e-1", "SID-1"),
				map[string]any{"element_id": "e-2", "content": "Sensitive body.", "id": nil, "text": "Sensitive body."}),
			"elements[1]"),
		Entry("a null role",
			ingestionDoc("1.3", sensitiveControl("e-1", "SID-1"),
				map[string]any{"element_id": "e-2", "content": "Sensitive body.", "id": "SID-2", "role": nil, "title": nil, "text": nil}),
			"elements[1]"),
		Entry("a blank title",
			ingestionDoc("1.3", sensitiveControl("e-1", "SID-1"),
				withRole(map[string]any{"element_id": "e-2", "content": "Sensitive body."}, "SID-2", "control", " ", "Sensitive body.", nil)),
			"elements[1] has a blank title"),
		Entry("a blank text",
			ingestionDoc("1.3", sensitiveControl("e-1", "SID-1"),
				withRole(map[string]any{"element_id": "e-2", "content": "Sensitive body."}, "SID-2", "control", "Sensitive title", "\t", nil)),
			"elements[1] has a blank text"),
		Entry("a blank metadata.title",
			withCatalogMeta(ingestionDoc("1.3", sensitiveControl("e-1", "SID-1")), " ", nil),
			"metadata.title is blank"),
		Entry("a blank metadata.source.catalog_version",
			withCatalogMeta(ingestionDoc("1.3", sensitiveControl("e-1", "SID-1")), "Sensitive catalog", "\n"),
			"metadata.source.catalog_version is blank"),
		Entry("a property name that is not a lowercase identifier",
			ingestionDoc("1.3", sensitiveControl("e-1", "SID-1"),
				withRole(map[string]any{"element_id": "e-2", "content": "Sensitive body."}, "SID-2", "control", nil, "Sensitive body.",
					map[string]string{"Sensitive_name": "Sensitive value"})),
			"elements[1] has a property"),
		Entry("a blank property value",
			ingestionDoc("1.3", sensitiveControl("e-1", "SID-1"),
				withRole(map[string]any{"element_id": "e-2", "content": "Sensitive body."}, "SID-2", "control", nil, "Sensitive body.",
					map[string]string{"level": " "})),
			"elements[1] has a property"),
		Entry("a role element whose parent has no id",
			ingestionDoc("1.3",
				heading("e-1", 1, nil, "Sensitive heading"),
				withRole(map[string]any{"element_id": "e-2", "content": "Sensitive body.", "parent_id": "e-1"}, "SID-2", "control", nil, "Sensitive body.", nil)),
			"elements[1]"),
	)

	DescribeTable("keeps today's path for output without roles",
		func(data []byte) {
			doc, meta, err := catalog.AdaptIngestionOutput(data)
			Expect(err).NotTo(HaveOccurred())
			Expect(meta.SourceItems).To(BeNil())
			Expect(meta.CatalogTitle).To(BeEmpty())
			Expect(meta.CatalogVersion).To(BeEmpty())
			Expect(doc.Sections).NotTo(BeEmpty())
		},
		Entry("1.2 output with source ids", withCatalogMeta(ingestionDoc("1.2",
			map[string]any{"element_id": "e-1", "type": "heading", "level": 1, "content": "V1", "id": "V1", "title": "Chapter"},
			map[string]any{"element_id": "e-2", "content": "Verify.", "parent_id": "e-1", "id": "V1.1", "properties": map[string]string{"level": "1"}},
		), "Test Catalog", "5.0.0")),
		Entry("Docling PDF output", mustReadFile("testdata/ingestion/nist-sp800-171r3-excerpt.json")),
		Entry("Docling HTML output", mustReadFile("testdata/ingestion/eurlex-nis2-32022L2555.json")),
	)

	It("adapts 1.2 output with source ids exactly as it does without them", func() {
		withIDs := ingestionDoc("1.2",
			map[string]any{"element_id": "e-1", "type": "heading", "level": 1, "content": "Chapter", "id": "V1", "title": "Chapter"},
			map[string]any{"element_id": "e-2", "content": "Verify.", "parent_id": "e-1", "id": "V1.1", "properties": map[string]string{"level": "1"}},
		)
		plain := ingestionDoc("1.2",
			heading("e-1", 1, nil, "Chapter"),
			map[string]any{"element_id": "e-2", "content": "Verify.", "parent_id": "e-1"},
		)
		got, _, err := catalog.AdaptIngestionOutput(withIDs)
		Expect(err).NotTo(HaveOccurred())
		want, _, err := catalog.AdaptIngestionOutput(plain)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(want))
	})
})

var _ = Describe("IsIngestionOutput", func() {
	DescribeTable("detects crosscodex-ingestion output",
		func(data []byte, want bool) {
			Expect(catalog.IsIngestionOutput(data)).To(Equal(want))
		},
		Entry("the PDF fixture", mustReadFile("testdata/ingestion/nist-sp800-171r3-excerpt.json"), true),
		Entry("the HTML fixture", mustReadFile("testdata/ingestion/eurlex-nis2-32022L2555.json"), true),
		Entry("output of an unsupported major", ingestionDoc("2.0"), true),
		Entry("an OSCAL catalog", mustReadFile("../../pkg/oscal/testdata/minimal_catalog.json"), false),
		Entry("an object without elements", []byte(`{"schema_version":"1.0"}`), false),
		Entry("an object without schema_version", []byte(`{"elements":[]}`), false),
		Entry("a JSON array", []byte(`[{"schema_version":"1.0","elements":[]}]`), false),
		Entry("plain text", []byte("schema_version elements"), false),
		Entry("PDF bytes", []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n1 0 obj\n<< /Type /Catalog >>\nendobj\n"), false),
		Entry("wrong-case keys", []byte(`{"SCHEMA_VERSION":"1.0","Elements":[]}`), false),
		Entry("empty input", []byte{}, false),
	)
})

func mustReadFile(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return data
}
