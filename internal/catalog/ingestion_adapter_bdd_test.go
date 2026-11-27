//go:build !integration

package catalog_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/storage"
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
		_, meta, err := catalog.AdaptIngestionOutput(withStructureMethod(ingestionDoc("1.9", sensitiveControl("e-1", "SID-1")), "heading"))
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
		Entry("1.3 output with no roles", ingestionDoc("1.3",
			heading("e-1", 1, nil, "Scope"), map[string]any{"element_id": "e-2", "content": "Body.", "parent_id": "e-1"})),
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

// roleFixtureElement is the id and role of a schema-1.3 element, decoded
// independently of the adapter.
type roleFixtureElement struct {
	ID   *string `json:"id"`
	Role string  `json:"role"`
}

var _ = Describe("AdaptIngestionOutput with real schema 1.3 output", func() {
	DescribeTable("names the source's own controls and groups",
		func(fixture, wantTitle, wantVersion string, wantContainers, wantControls int, wantSample string, wantProps bool) {
			data, _ := readIngestionFixture(fixture)
			var out struct {
				Elements []roleFixtureElement `json:"elements"`
			}
			Expect(json.Unmarshal(data, &out)).To(Succeed())
			var groupIDs, controlIDs []string
			for _, e := range out.Elements {
				switch {
				case e.ID == nil:
				case e.Role == "group":
					groupIDs = append(groupIDs, *e.ID)
				case e.Role == "control":
					controlIDs = append(controlIDs, *e.ID)
				}
			}
			Expect(groupIDs).To(HaveLen(wantContainers))
			Expect(controlIDs).To(HaveLen(wantControls))

			doc, meta, err := catalog.AdaptIngestionOutput(data)
			Expect(err).NotTo(HaveOccurred())
			Expect(doc).To(BeZero())
			Expect(meta.SchemaVersion).To(Equal("1.3"))
			Expect(meta.CatalogTitle).To(Equal(wantTitle))
			Expect(meta.CatalogVersion).To(Equal(wantVersion))

			tree, err := oscal.BuildTree(meta.SourceItems)
			Expect(err).NotTo(HaveOccurred())
			var gotContainers, gotControls []string
			for _, c := range tree.Containers {
				gotContainers = append(gotContainers, c.Identifier)
			}
			for _, it := range tree.Items {
				gotControls = append(gotControls, it.ID)
				Expect(it.Class).To(Equal(oscal.ClassRequirement))
				if wantProps {
					Expect(it.Props).To(HaveKey("level"), it.ID)
				}
			}
			Expect(gotContainers).To(ConsistOf(groupIDs))
			Expect(gotControls).To(Equal(controlIDs))
			Expect(gotControls).To(ContainElement(wantSample))
		},
		Entry("ASVS 5.0.0: chapters and sections are containers, requirements carry a level",
			"owasp-asvs-5.0.0-excerpt.json", "OWASP Application Security Verification Standard 5.0.0", "5.0.0", 11, 43, "V1.1.1", true),
		Entry("SP 800-53 Rev. 5: no containers",
			"nist-sp800-53r5-excerpt.json", "NIST SP 800-53 Revision 5 control catalog", "Revision 5", 0, 31, "AC-2(1)", false),
	)

	It("keeps ASVS V-prefixed requirements under their section and chapter", func() {
		data, _ := readIngestionFixture("owasp-asvs-5.0.0-excerpt.json")
		_, meta, err := catalog.AdaptIngestionOutput(data)
		Expect(err).NotTo(HaveOccurred())
		tree, err := oscal.BuildTree(meta.SourceItems)
		Expect(err).NotTo(HaveOccurred())

		identifiers := map[string]string{}
		for _, c := range tree.Containers {
			identifiers[c.Key] = c.Identifier
		}
		Expect(identifiers).To(ContainElements("V1", "V5", "V1.1", "V5.1"))
		for _, it := range tree.Items {
			Expect(it.ID).To(HavePrefix("V"))
			Expect(it.ContainerKey).NotTo(BeEmpty(), it.ID)
			Expect(it.ID).To(HavePrefix(identifiers[it.ContainerKey]+"."), it.ID)
		}
	})

	It("links SP 800-53 enhancements to their base control", func() {
		data, _ := readIngestionFixture("nist-sp800-53r5-excerpt.json")
		_, meta, err := catalog.AdaptIngestionOutput(data)
		Expect(err).NotTo(HaveOccurred())

		parents := map[string]string{}
		for _, it := range meta.SourceItems {
			parents[it.ID] = it.ParentID
		}
		Expect(parents).To(HaveKeyWithValue("AC-2", ""))
		Expect(parents).To(HaveKeyWithValue("AC-2(1)", "AC-2"))
		Expect(parents).To(HaveKeyWithValue("AC-3(2)", "AC-3"))
	})
})

// withStructureMethod sets metadata.structure_method on a document built by
// ingestionDoc (nil encodes null).
func withStructureMethod(data []byte, method any) []byte {
	var doc map[string]any
	Expect(json.Unmarshal(data, &doc)).To(Succeed())
	doc["metadata"].(map[string]any)["structure_method"] = method
	out, err := json.Marshal(doc)
	Expect(err).NotTo(HaveOccurred())
	return out
}

// withMappingRecord adds metadata.source.mapping to a document built by
// ingestionDoc.
func withMappingRecord(data []byte) []byte {
	var doc map[string]any
	Expect(json.Unmarshal(data, &doc)).To(Succeed())
	doc["metadata"].(map[string]any)["source"] = map[string]any{
		"kind": "structured", "format": "json",
		"mapping": map[string]any{"id": "m", "sha256": hashMappingSHA, "mapping_version": 1},
	}
	out, err := json.Marshal(doc)
	Expect(err).NotTo(HaveOccurred())
	return out
}

// sensitiveSection is an element that fails the rules only if its children or
// parent are wrong; its text and ids are canaries for the no-quoting check.
func sensitiveSection(elementID, id string, parent any) map[string]any {
	return withRole(map[string]any{"element_id": elementID, "type": "heading", "level": 1, "content": "Sensitive lead-in:", "parent_id": parent},
		id, "section", "Sensitive title", "Sensitive lead-in:", nil)
}

// childOf makes a control element with the given parent.
func childOf(elementID, id, parent string) map[string]any {
	return withRole(map[string]any{"element_id": elementID, "content": "Sensitive body.", "parent_id": parent},
		id, "control", nil, "Sensitive body.", nil)
}

// structureFixtureElement is the id, role and parent of a schema-1.4 element,
// decoded independently of the adapter.
type structureFixtureElement struct {
	ElementID string  `json:"element_id"`
	ParentID  *string `json:"parent_id"`
	ID        *string `json:"id"`
	Role      string  `json:"role"`
}

var _ = Describe("AdaptIngestionOutput with schema 1.4 structure", func() {
	validSection := func() []byte {
		return withStructureMethod(ingestionDoc("1.4",
			sensitiveSection("e-1", "SID-1", nil),
			map[string]any{"element_id": "e-2", "content": "Sensitive lead-in:", "parent_id": "e-1"},
			childOf("e-3", "SID-1.a", "e-1")), "heading")
	}

	It("maps a section to a ClassSection item that parents its children", func() {
		doc, meta, err := catalog.AdaptIngestionOutput(validSection())
		Expect(err).NotTo(HaveOccurred())
		Expect(doc).To(BeZero())
		Expect(meta.SchemaVersion).To(Equal("1.4"))
		Expect(meta.StructureMethod).To(Equal("heading"))
		Expect(meta.SourceItems).To(Equal([]oscal.ControlItem{
			{ID: "SID-1", Title: "Sensitive title", Text: "Sensitive lead-in:", Class: oscal.ClassSection},
			{ID: "SID-1.a", Text: "Sensitive body.", ParentID: "SID-1", Class: oscal.ClassRequirement},
		}))
		Expect(meta.SourceItems[0].Props).NotTo(HaveKey("node-kind"))
	})

	It("puts a section under a group by GroupID and under a section by ParentID", func() {
		data := withStructureMethod(withMappingRecord(ingestionDoc("1.4",
			withRole(heading("e-1", 1, nil, "Group"), "G1", "group", "Group", nil, nil),
			withRole(heading("e-2", 2, "e-1", "Outer"), "G1.s", "section", "Outer", nil, nil),
			withRole(heading("e-3", 3, "e-2", "Inner"), "G1.s.t", "section", "Inner", nil, nil),
			childOf("e-4", "G1.s.t.a", "e-3"))), "mapping")

		_, meta, err := catalog.AdaptIngestionOutput(data)
		Expect(err).NotTo(HaveOccurred())
		Expect(meta.StructureMethod).To(Equal("mapping"))
		parents := map[string][2]string{}
		for _, it := range meta.SourceItems {
			parents[it.ID] = [2]string{it.GroupID, it.ParentID}
		}
		Expect(parents).To(Equal(map[string][2]string{
			"G1": {"", ""}, "G1.s": {"G1", ""}, "G1.s.t": {"", "G1.s"}, "G1.s.t.a": {"", "G1.s.t"},
		}))
		_, err = oscal.BuildTree(meta.SourceItems)
		Expect(err).NotTo(HaveOccurred())
	})

	It("leaves StructureMethod empty for pre-1.4 output", func() {
		data, _ := readIngestionFixture("owasp-asvs-5.0.0-excerpt.json")
		_, meta, err := catalog.AdaptIngestionOutput(data)
		Expect(err).NotTo(HaveOccurred())
		Expect(meta.StructureMethod).To(BeEmpty())
	})

	DescribeTable("rejects output breaking the structure_method or section rules",
		func(data []byte, wantProblem string) {
			before := append([]byte(nil), data...)
			var (
				doc  oscal.StructuredDoc
				meta catalog.IngestionMeta
				err  error
			)
			Expect(func() { doc, meta, err = catalog.AdaptIngestionOutput(data) }).NotTo(Panic())
			Expect(err).To(MatchError(catalog.ErrInvalidIngestionOutput))
			Expect(err).To(MatchError(ContainSubstring(wantProblem)))
			Expect(err).To(MatchError(ContainSubstring("Re-run the conversion")))
			Expect(err.Error()).NotTo(ContainSubstring("Sensitive"))
			Expect(err.Error()).NotTo(ContainSubstring("SID-"))
			Expect(doc).To(BeZero())
			Expect(meta).To(BeZero())
			Expect(data).To(Equal(before))
		},
		Entry("1.4 with no method",
			ingestionDoc("1.4", sensitiveControl("e-1", "SID-1")),
			"metadata.structure_method is missing"),
		Entry("1.4 with a null method",
			withStructureMethod(ingestionDoc("1.4", sensitiveControl("e-1", "SID-1")), nil),
			"metadata.structure_method is missing"),
		Entry("an unknown method",
			withStructureMethod(ingestionDoc("1.4", sensitiveControl("e-1", "SID-1")), "Sensitive method"),
			"metadata.structure_method is not one of mapping, heading, table, pattern"),
		Entry("a method that is not a string",
			withStructureMethod(ingestionDoc("1.4", sensitiveControl("e-1", "SID-1")), 7),
			"metadata.structure_method is not valid JSON"),
		Entry("a method at 1.3",
			withStructureMethod(ingestionDoc("1.3", sensitiveControl("e-1", "SID-1")), "heading"),
			"metadata.structure_method is not allowed below schema 1.4"),
		Entry("a null method at 1.3",
			withStructureMethod(ingestionDoc("1.3", sensitiveControl("e-1", "SID-1")), nil),
			"metadata.structure_method is not allowed below schema 1.4"),
		Entry("mapping without source.mapping",
			withStructureMethod(ingestionDoc("1.4", sensitiveControl("e-1", "SID-1")), "mapping"),
			"is mapping but metadata.source.mapping is absent"),
		Entry("heading with source.mapping",
			withStructureMethod(withMappingRecord(ingestionDoc("1.4", sensitiveControl("e-1", "SID-1"))), "heading"),
			"metadata.source.mapping is present"),
		Entry("a section with no children",
			withStructureMethod(ingestionDoc("1.4", sensitiveControl("e-1", "SID-1"), sensitiveSection("e-2", "SID-2", nil)), "heading"),
			"elements[1] is a section with no child"),
		Entry("a section whose only child has no id",
			withStructureMethod(ingestionDoc("1.4", sensitiveSection("e-1", "SID-1", nil),
				map[string]any{"element_id": "e-2", "content": "Sensitive lead-in:", "parent_id": "e-1"}), "heading"),
			"elements[0] is a section with no child"),
		Entry("a section under a control",
			withStructureMethod(ingestionDoc("1.4",
				withRole(heading("e-1", 1, nil, "Sensitive heading"), "SID-1", "control", "Sensitive title", nil, nil),
				sensitiveSection("e-2", "SID-2", "e-1"),
				childOf("e-3", "SID-3", "e-2")), "heading"),
			"elements[1] is a section whose parent elements[0] is a control"),
		Entry("a group under a section",
			withStructureMethod(withMappingRecord(ingestionDoc("1.4",
				sensitiveSection("e-1", "SID-1", nil),
				withRole(heading("e-2", 2, "e-1", "Sensitive heading"), "SID-2", "group", "Sensitive title", nil, nil),
				childOf("e-3", "SID-3", "e-2"))), "mapping"),
			"elements[1] is a group whose parent elements[0] is a section"),
		Entry("1.4 output with no roles and method heading",
			withStructureMethod(ingestionDoc("1.4", heading("e-1", 1, nil, "Sensitive heading"),
				map[string]any{"element_id": "e-2", "content": "Sensitive body.", "parent_id": "e-1"}), "heading"),
			"schema 1.4 output has no element with a role"),
		Entry("1.4 output with no roles and method mapping",
			withStructureMethod(withMappingRecord(ingestionDoc("1.4", heading("e-1", 1, nil, "Sensitive heading"))), "mapping"),
			"schema 1.4 output has no element with a role"),
		Entry("a section at 1.3",
			ingestionDoc("1.3", sensitiveSection("e-1", "SID-1", nil), childOf("e-2", "SID-2", "e-1")),
			"elements[0] has the role section, which schema 1.4 introduced"),
	)

	DescribeTable("adapts the real schema 1.4 fixtures",
		func(fixture, wantTitleSubstring string, wantSections int) {
			data, _ := readIngestionFixture(fixture)
			var out struct {
				Elements []structureFixtureElement `json:"elements"`
			}
			Expect(json.Unmarshal(data, &out)).To(Succeed())
			idOf := map[string]string{}
			roleOf := map[string]string{}
			for _, e := range out.Elements {
				if e.ID != nil {
					idOf[e.ElementID], roleOf[e.ElementID] = *e.ID, e.Role
				}
			}
			var wantIDs []string
			wantParent := map[string]string{}
			sections := 0
			for _, e := range out.Elements {
				if e.ID == nil {
					continue
				}
				wantIDs = append(wantIDs, *e.ID)
				if e.ParentID != nil {
					wantParent[*e.ID] = idOf[*e.ParentID]
					Expect(roleOf[*e.ParentID]).To(Equal("section"), "only sections have children in %s", fixture)
				}
				if e.Role == "section" {
					sections++
				}
			}
			Expect(sections).To(Equal(wantSections))

			doc, meta, err := catalog.AdaptIngestionOutput(data)
			Expect(err).NotTo(HaveOccurred())
			Expect(doc).To(BeZero())
			Expect(meta.SchemaVersion).To(Equal("1.4"))
			Expect(meta.StructureMethod).To(Equal("heading"))
			Expect(meta.MappingID).To(BeEmpty())
			Expect(meta.CatalogTitle).To(ContainSubstring(wantTitleSubstring))

			var gotIDs []string
			gotParent := map[string]string{}
			gotSections := 0
			for _, it := range meta.SourceItems {
				gotIDs = append(gotIDs, it.ID)
				Expect(it.GroupID).To(BeEmpty(), it.ID)
				if it.ParentID != "" {
					gotParent[it.ID] = it.ParentID
				}
				if it.Class == oscal.ClassSection {
					gotSections++
				} else {
					Expect(it.Class).To(Equal(oscal.ClassRequirement), it.ID)
				}
			}
			Expect(gotIDs).To(Equal(wantIDs))
			Expect(gotParent).To(Equal(wantParent))
			Expect(gotSections).To(Equal(wantSections))
			_, err = oscal.BuildTree(meta.SourceItems)
			Expect(err).NotTo(HaveOccurred())
		},
		Entry("SP 800-171r3 PDF: flat controls, no section", "nist-sp800-171r3-excerpt-1.4.json", "", 0),
		Entry("NIS2 HTML: lead-in clauses are sections", "eurlex-nis2-32022L2555-1.4.json", "DIRECTIVE (EU) 2022/2555", 2),
	)

	It("hashes structure_method, so only the method changing changes the hash", func() {
		hashFor := func(method string) string {
			doc := ingestionDoc("1.4", sensitiveControl("e-1", "SID-1"))
			_, meta, err := catalog.AdaptIngestionOutput(withStructureMethod(doc, method))
			Expect(err).NotTo(HaveOccurred())
			return meta.ContentHash
		}
		Expect(hashFor("heading")).To(Equal(hashFor("heading")))
		Expect(hashFor("heading")).NotTo(Equal(hashFor("table")))
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

// contentHashDoc is the base document of the content-hash specs, assembled
// from named pieces so a spec can reorder or rewrite one of them.
const (
	hashMappingSHA = "de791a43657db03a3a728c4ec1bc14b31d400c0cfe70958fbe7d958847a0ec36"
	hashOtherSHA   = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	hashMapping    = `{"id":"m","sha256":"` + hashMappingSHA + `","mapping_version":1}`
	hashElements   = `[{"element_id":"e-1","type":"heading","content":"G","level":1,"parent_id":null,"id":"G1","role":"group","title":"G","text":null,"properties":{"k":"v"}},` +
		`{"element_id":"e-2","type":"paragraph","content":"C","level":null,"parent_id":"e-1","id":"G1.1","role":"control","title":null,"text":"C","properties":{"level":"1"}}]`
	hashMetadata = `{"title":"Cat","source":{"kind":"structured","node_count":2,"unmapped_fields":[],"catalog_version":"1.0","mapping":` + hashMapping + `},"processing_time_ms":9.4}`
	hashBase     = `{"schema_version":"1.3","elements":` + hashElements + `,"metadata":` + hashMetadata + `}`
)

// hashSwap returns a transform replacing the one occurrence of old in a
// document, failing the spec if old is absent so a typo cannot pass as "same".
func hashSwap(old, replacement string) func(string) string {
	return func(doc string) string {
		Expect(strings.Count(doc, old)).To(Equal(1), "%q must occur once in the base document", old)
		return strings.Replace(doc, old, replacement, 1)
	}
}

func contentHashOf(doc string) string {
	_, meta, err := catalog.AdaptIngestionOutput([]byte(doc))
	Expect(err).NotTo(HaveOccurred())
	return meta.ContentHash
}

var _ = Describe("content hash", func() {
	profile := func(sha string) string { return `{"id":"p","sha256":"` + sha + `"}` }
	withProfile := func(sha string) func(string) string {
		return hashSwap(`"processing_time_ms":9.4}`, `"processing_time_ms":9.4,"html_profile":`+profile(sha)+`}`)
	}

	DescribeTable("gives the same hash",
		func(transform func(string) string) {
			Expect(contentHashOf(transform(hashBase))).To(Equal(contentHashOf(hashBase)))
		},
		Entry("when processing_time_ms changes", hashSwap(`"processing_time_ms":9.4`, `"processing_time_ms":123.5`)),
		Entry("when top-level keys are reordered", func(string) string {
			return `{"metadata":` + hashMetadata + `,"elements":` + hashElements + `,"schema_version":"1.3"}`
		}),
		Entry("when whitespace is added", func(doc string) string {
			var out bytes.Buffer
			Expect(json.Indent(&out, []byte(doc), "", "    ")).To(Succeed())
			return out.String()
		}),
		Entry("when an unknown top-level key is added", hashSwap(`{"schema_version"`, `{"future":1,"schema_version"`)),
		Entry("when the mapping has unknown keys", hashSwap(`"mapping_version":1}`, `"mapping_version":1,"note":"x"}`)),
		Entry("when an element gains page and style", hashSwap(`"element_id":"e-1",`, `"element_id":"e-1","page":3,"style":{"b":true},`)),
		Entry("when source.node_count and unmapped_fields change", hashSwap(`"node_count":2,"unmapped_fields":[]`, `"node_count":9,"unmapped_fields":["x"]`)),
		Entry("when a duplicate title key ends on the same value", hashSwap(`"title":"Cat"`, `"title":"Other","title":"Cat"`)),
	)

	DescribeTable("gives a different hash",
		func(transform func(string) string) {
			Expect(contentHashOf(transform(hashBase))).NotTo(Equal(contentHashOf(hashBase)))
		},
		Entry("when element content changes", hashSwap(`"content":"C"`, `"content":"C2"`)),
		Entry("when an element role changes", hashSwap(`"role":"group"`, `"role":"control"`)),
		Entry("when an element title changes", hashSwap(`"title":"G"`, `"title":"H"`)),
		Entry("when a property value changes", hashSwap(`"k":"v"`, `"k":"w"`)),
		Entry("when catalog_version changes", hashSwap(`"catalog_version":"1.0"`, `"catalog_version":"2.0"`)),
		Entry("when metadata.title changes", hashSwap(`"title":"Cat"`, `"title":"Dog"`)),
		Entry("when the mapping sha256 changes", hashSwap(hashMappingSHA, hashOtherSHA)),
		Entry("when the mapping id changes", hashSwap(`"id":"m"`, `"id":"n"`)),
		Entry("when mapping_version changes", hashSwap(`"mapping_version":1`, `"mapping_version":2`)),
		Entry("when an html_profile is added", withProfile(hashMappingSHA)),
	)

	It("hashes the html_profile sha256", func() {
		Expect(contentHashOf(withProfile(hashMappingSHA)(hashBase))).NotTo(Equal(contentHashOf(withProfile(hashOtherSHA)(hashBase))))
	})

	It("reports the html_profile in the metadata, and none when absent", func() {
		_, with, err := catalog.AdaptIngestionOutput([]byte(withProfile(hashMappingSHA)(hashBase)))
		Expect(err).NotTo(HaveOccurred())
		Expect(with.HTMLProfileID).To(Equal("p"))
		Expect(with.HTMLProfileSHA256).To(Equal(hashMappingSHA))

		_, without, err := catalog.AdaptIngestionOutput([]byte(hashBase))
		Expect(err).NotTo(HaveOccurred())
		Expect(without.HTMLProfileID).To(BeEmpty())
		Expect(without.HTMLProfileSHA256).To(BeEmpty())
	})

	It("tells an absent element title from a null one", func() {
		absent := `{"schema_version":"1.3","elements":[{"element_id":"e-1","type":"paragraph","content":"x"}]}`
		null := `{"schema_version":"1.3","elements":[{"element_id":"e-1","type":"paragraph","content":"x","title":null}]}`
		Expect(contentHashOf(absent)).NotTo(Equal(contentHashOf(null)))
	})

	DescribeTable("hashes role-less output",
		func(fixture string) {
			data, _ := readIngestionFixture(fixture)
			_, meta, err := catalog.AdaptIngestionOutput(data)
			Expect(err).NotTo(HaveOccurred())
			Expect(meta.ContentHash).To(MatchRegexp(`^[0-9a-f]{64}$`))

			retimed := regexp.MustCompile(`"processing_time_ms":\s*[0-9.eE+-]+`).ReplaceAll(data, []byte(`"processing_time_ms": 123456.789`))
			Expect(retimed).NotTo(Equal(data), "the fixture must carry processing_time_ms for this spec to mean anything")
			_, again, err := catalog.AdaptIngestionOutput(retimed)
			Expect(err).NotTo(HaveOccurred())
			Expect(again.ContentHash).To(Equal(meta.ContentHash))
		},
		Entry("the NIS2 HTML fixture", "eurlex-nis2-32022L2555.json"),
		Entry("the SP 800-171r3 PDF fixture", "nist-sp800-171r3-excerpt.json"),
	)

	DescribeTable("ContentIdentity hashes the bytes of anything that is not ingestion output",
		func(content string) {
			kind, hash, err := catalog.ContentIdentity([]byte(content))
			Expect(err).NotTo(HaveOccurred())
			Expect(kind).To(Equal(catalog.ContentKindBytes))
			Expect(hash).To(Equal(storage.ContentHash([]byte(content))))
		},
		Entry("plain text", "not json at all"),
		Entry("JSON without elements", `{"schema_version":"1.3"}`),
		Entry("an OSCAL catalog", `{"catalog":{"uuid":"u","metadata":{"title":"t"}}}`),
		Entry("an OSCAL catalog that also looks like ingestion output",
			`{"catalog":{"uuid":"u","metadata":{"title":"t"}},"schema_version":"1","elements":[]}`),
	)

	It("ContentIdentity returns the adapter's hash and error for ingestion output", func() {
		data, _ := readIngestionFixture("nist-sp800-171r3-excerpt.json")
		_, meta, err := catalog.AdaptIngestionOutput(data)
		Expect(err).NotTo(HaveOccurred())
		kind, hash, err := catalog.ContentIdentity(data)
		Expect(err).NotTo(HaveOccurred())
		Expect(kind).To(Equal(catalog.ContentKindIngestion))
		Expect(hash).To(Equal(meta.ContentHash))

		kind, hash, err = catalog.ContentIdentity([]byte(`{"schema_version":"1.3","elements":[]}`))
		Expect(err).To(MatchError(catalog.ErrInvalidIngestionOutput))
		Expect(kind).To(BeEmpty())
		Expect(hash).To(BeEmpty())
	})

	DescribeTable("exposes the mapping",
		func(fixture, wantID, wantSHA string) {
			data, _ := readIngestionFixture(fixture)
			_, meta, err := catalog.AdaptIngestionOutput(data)
			Expect(err).NotTo(HaveOccurred())
			Expect(meta.MappingID).To(Equal(wantID))
			Expect(meta.MappingSHA256).To(Equal(wantSHA))
		},
		Entry("an ASVS fixture with a mapping", "owasp-asvs-5.0.0-excerpt.json", "owasp-asvs-5.0.0-json", "de791a43657db03a3a728c4ec1bc14b31d400c0cfe70958fbe7d958847a0ec36"),
		Entry("an SP 800-171r3 fixture without one", "nist-sp800-171r3-excerpt.json", "", ""),
	)

	DescribeTable("rejects a malformed mapping or profile",
		func(transform func(string) string, path string) {
			_, _, err := catalog.AdaptIngestionOutput([]byte(transform(hashBase)))
			Expect(err).To(MatchError(catalog.ErrInvalidIngestionOutput))
			Expect(err.Error()).To(ContainSubstring(path))
			Expect(err.Error()).To(ContainSubstring("Re-run the conversion"))
		},
		Entry("a null mapping", hashSwap(hashMapping, `null`), "metadata.source.mapping"),
		Entry("a string mapping", hashSwap(hashMapping, `"m"`), "metadata.source.mapping"),
		Entry("a mapping without id", hashSwap(`"id":"m",`, ``), "metadata.source.mapping.id"),
		Entry("a mapping without sha256", hashSwap(`"sha256":"`+hashMappingSHA+`",`, ``), "metadata.source.mapping.sha256"),
		Entry("a mapping without mapping_version", hashSwap(`,"mapping_version":1`, ``), "metadata.source.mapping.mapping_version"),
		Entry("a blank mapping id", hashSwap(`"id":"m"`, `"id":"  "`), "metadata.source.mapping.id"),
		Entry("an uppercase mapping sha256", hashSwap(hashMappingSHA, strings.ToUpper(hashMappingSHA)), "metadata.source.mapping.sha256"),
		Entry("a 63-character mapping sha256", hashSwap(hashMappingSHA, hashMappingSHA[:63]), "metadata.source.mapping.sha256"),
		Entry("mapping_version 0", hashSwap(`"mapping_version":1`, `"mapping_version":0`), "metadata.source.mapping.mapping_version"),
		Entry("a fractional mapping_version", hashSwap(`"mapping_version":1`, `"mapping_version":1.5`), "metadata.source.mapping.mapping_version"),
		Entry("a null html_profile", hashSwap(`"processing_time_ms":9.4}`, `"processing_time_ms":9.4,"html_profile":null}`), "metadata.html_profile"),
		Entry("an html_profile without id", hashSwap(`"processing_time_ms":9.4}`, `"processing_time_ms":9.4,"html_profile":{"sha256":"`+hashMappingSHA+`"}}`), "metadata.html_profile.id"),
		Entry("a malformed html_profile sha256", hashSwap(`"processing_time_ms":9.4}`, `"processing_time_ms":9.4,"html_profile":`+profile("abc")+`}`), "metadata.html_profile.sha256"),
	)

	DescribeTable("refuses an html_profile id outside ingestion's profile-id rule, without echoing it",
		func(id string, schema string) {
			quoted, err := json.Marshal(id)
			Expect(err).NotTo(HaveOccurred())
			doc := strings.Replace(hashBase, `"schema_version":"1.3"`, `"schema_version":"`+schema+`"`, 1)
			doc = hashSwap(`"processing_time_ms":9.4}`,
				`"processing_time_ms":9.4,"html_profile":{"id":`+string(quoted)+`,"sha256":"`+hashMappingSHA+`"}}`)(doc)

			_, _, err = catalog.AdaptIngestionOutput([]byte(doc))

			Expect(err).To(MatchError(catalog.ErrInvalidIngestionOutput))
			Expect(err).To(MatchError(ContainSubstring("metadata.html_profile.id")))
			Expect(err).To(MatchError(ContainSubstring("Re-run the conversion")))
			Expect(err.Error()).NotTo(ContainSubstring(id))
		},
		Entry("an uppercase id", "EUR-LEX", "1.3"),
		Entry("an id with a space", "eur lex", "1.3"),
		Entry("an id with a control character", "eur\u0007lex", "1.3"),
		Entry("a leading hyphen", "-eur-lex", "1.3"),
		Entry("129 bytes", strings.Repeat("a", 129), "1.3"),
		Entry("an uppercase id at schema 1.2", "EUR-LEX", "1.2"),
	)

	It("accepts an html_profile id of exactly 128 bytes", func() {
		id := strings.Repeat("a", 128)
		doc := hashSwap(`"processing_time_ms":9.4}`,
			`"processing_time_ms":9.4,"html_profile":{"id":"`+id+`","sha256":"`+hashMappingSHA+`"}}`)(hashBase)

		_, meta, err := catalog.AdaptIngestionOutput([]byte(doc))

		Expect(err).NotTo(HaveOccurred())
		Expect(meta.HTMLProfileID).To(Equal(id))
	})

	DescribeTable("pins the v1 hash of each fixture",
		func(fixture, want string) {
			data, _ := readIngestionFixture(fixture)
			_, meta, err := catalog.AdaptIngestionOutput(data)
			Expect(err).NotTo(HaveOccurred())
			Expect(meta.ContentHash).To(Equal(want))
		},
		Entry("the NIS2 HTML fixture", "eurlex-nis2-32022L2555.json",
			"08129ed7d8dde5fbe766064bc8816aaf690dc585917fcd4ce1d397504892099a"),
		Entry("the SP 800-171r3 PDF fixture", "nist-sp800-171r3-excerpt.json",
			"4972a9e2932b12f9a072daed8cb733ad51f0cc70da2194bd8e242cdd73cff3cf"),
		Entry("the SP 800-53r5 fixture", "nist-sp800-53r5-excerpt.json",
			"b55783a79ae22fd5e5a67a643eac65046d2f2342ba95413de0cd214684bf50a3"),
		Entry("the ASVS fixture", "owasp-asvs-5.0.0-excerpt.json",
			"a19a64cad1ad2bf35b634af7579baa3b8e23106760dc02ffca858626ec977231"),
	)
})

func mustReadFile(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return data
}
