package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

// This file adapts crosscodex-ingestion's structured output (the JSON document
// at a conversion's structured_json_uri) to oscal.StructuredDoc. The contract
// is the ingestion repo's docs/usage/output-format.md and its JSON Schema
// structured-output-v1.json. The output comes from another service through
// object storage, so it is untrusted: AdaptIngestionOutput checks every rule
// it relies on before building anything.

// Ingestion output schema rules this adapter is written against (schema v1).
const (
	// supportedIngestionMajor is the schema_version major this adapter reads.
	// A minor only adds optional fields, which are ignored, so any 1.x reads.
	supportedIngestionMajor = 1
	// maxIngestionHeadingLevel is the deepest heading level schema v1 allows.
	maxIngestionHeadingLevel = 100
)

// Element types defined by ingestion output schema v1. A new type is a major
// version change, so any other value is rejected.
const (
	ingestionTypeHeading   = "heading"
	ingestionTypeParagraph = "paragraph"
	ingestionTypeListItem  = "list_item"
	ingestionTypeTable     = "table"
)

var (
	// ErrUnsupportedSchema reports ingestion output whose schema_version is
	// missing, malformed, or of a major this server does not read.
	ErrUnsupportedSchema = errors.New("unsupported ingestion output schema")
	// ErrInvalidIngestionOutput reports ingestion output that breaks a rule of
	// the schema it declares.
	ErrInvalidIngestionOutput = errors.New("invalid ingestion output")
)

var (
	ingestionVersionPattern = regexp.MustCompile(`^(\d+)\.(\d+)$`)
	// markdownSeparatorCell matches one cell of a GFM table delimiter row.
	markdownSeparatorCell = regexp.MustCompile(`^:?-+:?$`)
)

// IngestionMeta describes the ingestion output a StructuredDoc came from.
type IngestionMeta struct {
	// SchemaVersion is the output's schema_version, MAJOR.MINOR.
	SchemaVersion string
}

// ingestionOutput and ingestionElement hold the schema v1 fields the adapter
// reads. decodeIngestionOutput fills them by exact key; they carry no json
// tags because encoding/json matches struct tags case-insensitively.
type ingestionOutput struct {
	SchemaVersion *string
	Elements      []ingestionElement
}

type ingestionElement struct {
	ElementID string
	Type      string
	Content   string
	Level     *int
	ParentID  *string
}

// decodeIngestionOutput decodes data into the fields the adapter reads,
// matching schema keys exactly. A key that differs only in case (Level,
// SCHEMA_VERSION) is not the schema key, so like every unknown key it is
// ignored rather than folded onto the real one.
func decodeIngestionOutput(data []byte) (ingestionOutput, error) {
	var out ingestionOutput
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return out, invalidIngestionJSON("the document", err)
	}
	if err := decodeIngestionField(top, "schema_version", &out.SchemaVersion); err != nil {
		return out, invalidIngestionJSON("schema_version", err)
	}
	var elements []map[string]json.RawMessage
	if err := decodeIngestionField(top, "elements", &elements); err != nil {
		return out, invalidIngestionJSON("elements", err)
	}
	out.Elements = make([]ingestionElement, len(elements))
	for i, raw := range elements {
		e := &out.Elements[i]
		fields := []struct {
			key string
			dst any
		}{
			{"element_id", &e.ElementID},
			{"type", &e.Type},
			{"content", &e.Content},
			{"level", &e.Level},
			{"parent_id", &e.ParentID},
		}
		for _, f := range fields {
			if err := decodeIngestionField(raw, f.key, f.dst); err != nil {
				return out, invalidIngestionJSON(fmt.Sprintf("elements[%d].%s", i, f.key), err)
			}
		}
	}
	return out, nil
}

// decodeIngestionField decodes obj[key] into dst; an absent key leaves dst
// unchanged.
func decodeIngestionField(obj map[string]json.RawMessage, key string, dst any) error {
	raw, ok := obj[key]
	if !ok {
		return nil
	}
	return json.Unmarshal(raw, dst)
}

func invalidIngestionJSON(where string, err error) error {
	return fmt.Errorf(
		"%w: %s is not valid JSON of the ingestion output shape (%v). Re-run the conversion; if it fails again, report it as an ingestion service bug",
		ErrInvalidIngestionOutput, where, err)
}

// IsIngestionOutput reports whether data is a JSON object with the
// schema_version and elements keys of crosscodex-ingestion output. It does not
// validate the document; AdaptIngestionOutput does.
func IsIngestionOutput(data []byte) bool {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}
	_, hasVersion := probe["schema_version"]
	_, hasElements := probe["elements"]
	return hasVersion && hasElements
}

// AdaptIngestionOutput decodes crosscodex-ingestion structured output and maps
// it to an oscal.StructuredDoc:
//   - each heading becomes a Section, in document order, with its level; its
//     Text is the content of the non-heading elements whose parent_id is that
//     heading, joined by blank lines;
//   - each table's Markdown becomes a Table: its first block of "|" lines
//     with a header and delimiter row. Lines before it (a caption) and after
//     it (trailing text or a further block) are not in the Table but stay in
//     Section Text and RawText;
//   - RawText is every element's content in document order, joined by blank
//     lines.
//
// Fields the adapter does not know are ignored, as the schema requires of
// readers; keys match exactly, so a wrong-case key is an unknown field. A
// schema_version with another major returns ErrUnsupportedSchema;
// output that breaks a schema rule the adapter relies on returns
// ErrInvalidIngestionOutput. Error messages name elements by index and never
// quote document text.
func AdaptIngestionOutput(data []byte) (oscal.StructuredDoc, IngestionMeta, error) {
	out, err := decodeIngestionOutput(data)
	if err != nil {
		return oscal.StructuredDoc{}, IngestionMeta{}, err
	}
	version, err := checkIngestionVersion(out.SchemaVersion)
	if err != nil {
		return oscal.StructuredDoc{}, IngestionMeta{}, err
	}
	if len(out.Elements) == 0 {
		return oscal.StructuredDoc{}, IngestionMeta{}, fmt.Errorf(
			"%w: the output has no elements, so there is no content to structure into a catalog. Check that the source document has text (a scanned image without OCR text converts to nothing) and convert it again",
			ErrInvalidIngestionOutput)
	}

	var doc oscal.StructuredDoc
	sectionOf := make(map[string]int, len(out.Elements)) // heading element_id -> index in doc.Sections
	seen := make(map[string]bool, len(out.Elements))
	sectionText := make([][]string, 0)
	raw := make([]string, 0, len(out.Elements))
	for i, e := range out.Elements {
		if err := checkIngestionElement(i, e, seen, sectionOf); err != nil {
			return oscal.StructuredDoc{}, IngestionMeta{}, err
		}
		seen[e.ElementID] = true
		raw = append(raw, e.Content)

		if e.Type == ingestionTypeHeading {
			sectionOf[e.ElementID] = len(doc.Sections)
			doc.Sections = append(doc.Sections, oscal.DocSection{Level: *e.Level, Title: e.Content})
			sectionText = append(sectionText, nil)
			continue
		}
		if e.Type == ingestionTypeTable {
			table, err := parseMarkdownTable(i, e.Content)
			if err != nil {
				return oscal.StructuredDoc{}, IngestionMeta{}, err
			}
			doc.Tables = append(doc.Tables, table)
		}
		if e.ParentID != nil {
			s := sectionOf[*e.ParentID]
			sectionText[s] = append(sectionText[s], e.Content)
		}
	}
	for i := range doc.Sections {
		doc.Sections[i].Text = strings.Join(sectionText[i], "\n\n")
	}
	doc.RawText = strings.Join(raw, "\n\n")
	return doc, IngestionMeta{SchemaVersion: version}, nil
}

func checkIngestionVersion(v *string) (string, error) {
	if v == nil {
		return "", fmt.Errorf(
			"%w: schema_version is not MAJOR.MINOR (it is missing). This server reads ingestion output schema major %d; re-run conversion with a matching ingestion version",
			ErrUnsupportedSchema, supportedIngestionMajor)
	}
	m := ingestionVersionPattern.FindStringSubmatch(*v)
	if m == nil {
		return "", fmt.Errorf(
			"%w: schema_version %.32q is not MAJOR.MINOR. This server reads ingestion output schema major %d; re-run conversion with a matching ingestion version",
			ErrUnsupportedSchema, *v, supportedIngestionMajor)
	}
	major, err := strconv.Atoi(m[1])
	if err != nil || major != supportedIngestionMajor {
		return "", fmt.Errorf(
			"%w: schema_version %.32q has major %.16s, but this server reads major %d; re-run conversion with a matching ingestion version",
			ErrUnsupportedSchema, *v, m[1], supportedIngestionMajor)
	}
	return *v, nil
}

// checkIngestionElement enforces the schema v1 rules the adapter relies on for
// elements[i]. seen holds the element_ids before it; sectionOf the headings
// among them.
func checkIngestionElement(i int, e ingestionElement, seen map[string]bool, sectionOf map[string]int) error {
	switch e.Type {
	case ingestionTypeHeading, ingestionTypeParagraph, ingestionTypeListItem, ingestionTypeTable:
	default:
		return fmt.Errorf(
			"%w: elements[%d] has type %.32q; schema v1 defines only heading, paragraph, list_item and table. Re-run conversion with a matching ingestion version",
			ErrInvalidIngestionOutput, i, e.Type)
	}
	if e.ElementID == "" {
		return fmt.Errorf("%w: elements[%d] has no element_id; every element needs a unique one. Re-run the conversion",
			ErrInvalidIngestionOutput, i)
	}
	if seen[e.ElementID] {
		return fmt.Errorf("%w: elements[%d] repeats the element_id of an earlier element; element_ids must be unique. Re-run the conversion",
			ErrInvalidIngestionOutput, i)
	}
	if strings.TrimSpace(e.Content) == "" {
		return fmt.Errorf("%w: elements[%d] has blank content; schema v1 never emits blank elements. Re-run the conversion",
			ErrInvalidIngestionOutput, i)
	}
	switch {
	case e.Type == ingestionTypeHeading && e.Level == nil:
		return fmt.Errorf("%w: elements[%d] is a heading with level null; a heading needs a level from 1 to %d. Re-run the conversion",
			ErrInvalidIngestionOutput, i, maxIngestionHeadingLevel)
	case e.Type == ingestionTypeHeading && (*e.Level < 1 || *e.Level > maxIngestionHeadingLevel):
		return fmt.Errorf("%w: elements[%d] is a heading with level %d, outside 1 to %d. Re-run the conversion",
			ErrInvalidIngestionOutput, i, *e.Level, maxIngestionHeadingLevel)
	case e.Type != ingestionTypeHeading && e.Level != nil:
		return fmt.Errorf("%w: elements[%d] is a %s with level %d; only headings have a level. Re-run the conversion",
			ErrInvalidIngestionOutput, i, e.Type, *e.Level)
	}
	if e.ParentID != nil {
		if _, ok := sectionOf[*e.ParentID]; !ok {
			return fmt.Errorf("%w: elements[%d] has a parent_id that is not an earlier heading; parent_id must name the heading the element sits under. Re-run the conversion",
				ErrInvalidIngestionOutput, i)
		}
	}
	return nil
}

// parseMarkdownTable parses the first GFM table block in a table element's
// content: a header row, a delimiter row and body rows, each a line starting
// with "|", up to the first other line. Lines outside that block are not
// parsed. The delimiter and every body row must have as many cells as the
// header.
func parseMarkdownTable(i int, content string) (oscal.DocTable, error) {
	lines := strings.Split(content, "\n")
	start := -1
	for j := 0; j+1 < len(lines); j++ {
		if isMarkdownRow(lines[j]) && isMarkdownSeparator(lines[j+1]) {
			start = j
			break
		}
	}
	if start < 0 {
		return oscal.DocTable{}, fmt.Errorf(
			"%w: elements[%d] is a table with no Markdown header row (a \"| ... |\" line followed by a \"|---|\" line); schema v1 tables are Markdown. Re-run the conversion",
			ErrInvalidIngestionOutput, i)
	}
	table := oscal.DocTable{Headers: splitMarkdownRow(lines[start])}
	if delimiter := splitMarkdownRow(lines[start+1]); len(delimiter) != len(table.Headers) {
		return oscal.DocTable{}, fmt.Errorf(
			"%w: elements[%d] is a table whose delimiter row has %d cells, but its header has %d. Re-run the conversion",
			ErrInvalidIngestionOutput, i, len(delimiter), len(table.Headers))
	}
	for _, line := range lines[start+2:] {
		if !isMarkdownRow(line) {
			break
		}
		row := splitMarkdownRow(line)
		if len(row) != len(table.Headers) {
			return oscal.DocTable{}, fmt.Errorf(
				"%w: elements[%d] is a table whose row %d has %d cells, but its header has %d; the adapter does not guess which cells are missing. Re-run the conversion",
				ErrInvalidIngestionOutput, i, len(table.Rows)+1, len(row), len(table.Headers))
		}
		table.Rows = append(table.Rows, row)
	}
	return table, nil
}

func isMarkdownRow(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "|")
}

func isMarkdownSeparator(line string) bool {
	if !isMarkdownRow(line) {
		return false
	}
	for _, cell := range splitMarkdownRow(line) {
		if !markdownSeparatorCell.MatchString(cell) {
			return false
		}
	}
	return true
}

// splitMarkdownRow splits a "| a | b |" line into trimmed cells. A "\|" is a
// literal pipe inside a cell.
func splitMarkdownRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	if strings.HasSuffix(line, "|") && !strings.HasSuffix(line, `\|`) {
		line = line[:len(line)-1]
	}
	var cells []string
	var cell strings.Builder
	for j := 0; j < len(line); j++ {
		switch {
		case line[j] == '\\' && j+1 < len(line) && line[j+1] == '|':
			cell.WriteByte('|')
			j++
		case line[j] == '|':
			cells = append(cells, strings.TrimSpace(cell.String()))
			cell.Reset()
		default:
			cell.WriteByte(line[j])
		}
	}
	return append(cells, strings.TrimSpace(cell.String()))
}
