//go:build !integration

package catalog_test

import (
	"encoding/json"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/catalog"
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
		Entry("a table whose delimiter row has a different number of cells",
			ingestionDoc("1.2", map[string]any{"type": "table", "content": "| a | b |\n|---|\n| 1 | 2 |"}),
			catalog.ErrInvalidIngestionOutput, "elements[0] is a table whose delimiter row has 1 cells, but its header has 2"),
		Entry("a table row with a different number of cells",
			ingestionDoc("1.2", map[string]any{"type": "table", "content": "| a | b |\n|---|---|\n| 1 |"}),
			catalog.ErrInvalidIngestionOutput, "elements[0] is a table whose row 1 has 1 cells, but its header has 2"),
	)
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
