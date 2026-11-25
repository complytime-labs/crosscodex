package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
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
	// ingestionPropertyName is the schema's rule for element property names.
	ingestionPropertyName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// IngestionMeta describes the ingestion output a StructuredDoc came from.
type IngestionMeta struct {
	// SchemaVersion is the output's schema_version, MAJOR.MINOR.
	SchemaVersion string
	// SourceItems is non-nil exactly when the output carries element roles
	// (schema 1.3 and later): one item per element with an id, in element
	// order, holding the source's own ID, title, text and properties. The
	// StructuredDoc returned with it is then zero. A role "control" item has
	// Class ClassRequirement; a role "group" item has no Class and becomes a
	// Container through oscal.BuildTree, because the items under it name it as
	// their GroupID. An item under a control names it as ParentID.
	SourceItems []oscal.ControlItem
	// CatalogTitle is metadata.title, the catalog's name as the conversion's
	// mapping gives it; "" when null or absent. Set only with SourceItems.
	CatalogTitle string
	// CatalogVersion is metadata.source.catalog_version; "" when null or
	// absent. Set only with SourceItems.
	CatalogVersion string
}

// ingestionOutput and ingestionElement hold the schema v1 fields the adapter
// reads. decodeIngestionOutput fills them by exact key; they carry no json
// tags because encoding/json matches struct tags case-insensitively.
type ingestionOutput struct {
	SchemaVersion  *string
	Elements       []ingestionElement
	CatalogTitle   *string // metadata.title
	CatalogVersion *string // metadata.source.catalog_version (since 1.3)
}

// The source-node fields (since 1.1 and 1.3) are read only on the source-ID
// path. The schema distinguishes an absent role, title or text key from a
// null value, so their presence is recorded apart from the value.
type ingestionElement struct {
	ElementID  string
	Type       string
	Content    string
	Level      *int
	ParentID   *string
	ID         *string
	Role       *string
	Title      *string
	Text       *string
	Properties map[string]string
	HasRole    bool
	HasTitle   bool
	HasText    bool
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
	var metadata, source map[string]json.RawMessage
	if err := decodeIngestionField(top, "metadata", &metadata); err != nil {
		return out, invalidIngestionJSON("metadata", err)
	}
	if err := decodeIngestionField(metadata, "title", &out.CatalogTitle); err != nil {
		return out, invalidIngestionJSON("metadata.title", err)
	}
	if err := decodeIngestionField(metadata, "source", &source); err != nil {
		return out, invalidIngestionJSON("metadata.source", err)
	}
	if err := decodeIngestionField(source, "catalog_version", &out.CatalogVersion); err != nil {
		return out, invalidIngestionJSON("metadata.source.catalog_version", err)
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
			{"id", &e.ID},
			{"role", &e.Role},
			{"title", &e.Title},
			{"text", &e.Text},
			{"properties", &e.Properties},
		}
		for _, f := range fields {
			if err := decodeIngestionField(raw, f.key, f.dst); err != nil {
				return out, invalidIngestionJSON(fmt.Sprintf("elements[%d].%s", i, f.key), err)
			}
		}
		_, e.HasRole = raw["role"]
		_, e.HasTitle = raw["title"]
		_, e.HasText = raw["text"]
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

// AdaptIngestionOutput decodes crosscodex-ingestion structured output.
//
// When any element has a role key (schema 1.3 and later), the output is a
// structured source whose structure is taken as given (the source-ID path):
// the StructuredDoc is zero and IngestionMeta carries SourceItems,
// CatalogTitle and CatalogVersion. Each element with an id becomes one item
// with that id, title, text and properties; id-less elements (text kept for
// older readers) are skipped. An item whose parent element is a control names
// it as ParentID; one whose parent is a group names it as GroupID. Output that
// breaks the role contract (an id without role, title or text key, a role
// without an id, a role other than group or control, a parent without an id,
// a group under a control, a group without children, a blank id, title, text,
// metadata.title or catalog_version, a property name that is not a lowercase
// identifier or a blank property value) returns ErrInvalidIngestionOutput.
//
// Otherwise it maps the output to an oscal.StructuredDoc:
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
	if slices.ContainsFunc(out.Elements, func(e ingestionElement) bool { return e.HasRole }) {
		items, err := sourceItems(out.Elements)
		if err != nil {
			return oscal.StructuredDoc{}, IngestionMeta{}, err
		}
		meta := IngestionMeta{SchemaVersion: version, SourceItems: items}
		for _, f := range []struct {
			key   string
			value *string
			dst   *string
		}{
			{"metadata.title", out.CatalogTitle, &meta.CatalogTitle},
			{"metadata.source.catalog_version", out.CatalogVersion, &meta.CatalogVersion},
		} {
			if f.value == nil {
				continue
			}
			if strings.TrimSpace(*f.value) == "" {
				return oscal.StructuredDoc{}, IngestionMeta{}, fmt.Errorf("%w: %s is blank; in output with roles it is a non-blank string or null. Re-run the conversion",
					ErrInvalidIngestionOutput, f.key)
			}
			*f.dst = *f.value
		}
		return oscal.StructuredDoc{}, meta, nil
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

// Element roles defined by ingestion output schema 1.3.
const (
	ingestionRoleGroup   = "group"
	ingestionRoleControl = "control"
)

// sourceItems maps the elements of output that carries roles to one
// ControlItem per element with an id, in element order, after checking the
// schema v1 rules (checkIngestionElement) and the 1.3 role contract. Ingestion
// emits parents before children, and checkIngestionElement guarantees a
// parent_id names an earlier element.
func sourceItems(elements []ingestionElement) ([]oscal.ControlItem, error) {
	seen := make(map[string]bool, len(elements))
	sectionOf := make(map[string]int)
	itemOf := make(map[string]int, len(elements)) // element_id -> index in items
	items := make([]oscal.ControlItem, 0, len(elements))
	isGroup := make([]bool, 0, len(elements))
	elementOf := make([]int, 0, len(elements)) // item index -> element index
	children := make([]int, 0, len(elements))
	for i, e := range elements {
		if err := checkIngestionElement(i, e, seen, sectionOf); err != nil {
			return nil, err
		}
		seen[e.ElementID] = true
		if e.Type == ingestionTypeHeading {
			sectionOf[e.ElementID] = len(sectionOf)
		}

		if e.ID == nil {
			if e.HasRole || e.HasText {
				return nil, fmt.Errorf("%w: elements[%d] has a role or text but no id; schema 1.3 allows them only on an element with a source id. Re-run the conversion",
					ErrInvalidIngestionOutput, i)
			}
			continue
		}
		if strings.TrimSpace(*e.ID) == "" {
			return nil, fmt.Errorf("%w: elements[%d] has a blank id; a source id is a non-blank string or null. Re-run the conversion",
				ErrInvalidIngestionOutput, i)
		}
		if !e.HasRole || !e.HasTitle || !e.HasText {
			return nil, fmt.Errorf("%w: elements[%d] has an id but lacks the role, title or text key; in output with roles every element with an id carries all three (title and text may be null). Re-run the conversion with a matching ingestion version",
				ErrInvalidIngestionOutput, i)
		}
		if e.Role == nil || (*e.Role != ingestionRoleGroup && *e.Role != ingestionRoleControl) {
			return nil, fmt.Errorf("%w: elements[%d] has a role other than group or control, the only roles schema 1.3 defines. Re-run the conversion with a matching ingestion version",
				ErrInvalidIngestionOutput, i)
		}
		group := *e.Role == ingestionRoleGroup
		if group && e.Title == nil {
			return nil, fmt.Errorf("%w: elements[%d] is a group with a null title; a group needs a title. Re-run the conversion or fix the mapping",
				ErrInvalidIngestionOutput, i)
		}

		item := oscal.ControlItem{ID: *e.ID, Props: e.Properties}
		for _, f := range []struct {
			key   string
			value *string
			dst   *string
		}{{"title", e.Title, &item.Title}, {"text", e.Text, &item.Text}} {
			if f.value == nil {
				continue
			}
			if strings.TrimSpace(*f.value) == "" {
				return nil, fmt.Errorf("%w: elements[%d] has a blank %s; schema 1.3 makes it a non-blank string or null. Re-run the conversion",
					ErrInvalidIngestionOutput, i, f.key)
			}
			*f.dst = *f.value
		}
		for name, value := range e.Properties {
			if !ingestionPropertyName.MatchString(name) || strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("%w: elements[%d] has a property whose name is not a lowercase identifier (a-z, 0-9, _; at most 64 characters) or whose value is blank. Re-run the conversion",
					ErrInvalidIngestionOutput, i)
			}
		}
		if !group {
			item.Class = oscal.ClassRequirement
		}
		if e.ParentID != nil {
			p, ok := itemOf[*e.ParentID]
			switch {
			case !ok:
				return nil, fmt.Errorf("%w: elements[%d] has a role, but its parent element has no id; in output with roles a source node's parent is a source node. Re-run the conversion",
					ErrInvalidIngestionOutput, i)
			case isGroup[p]:
				item.GroupID = items[p].ID
			case group:
				return nil, fmt.Errorf("%w: elements[%d] is a group whose parent elements[%d] is a control; groups nest only in groups. Re-run the conversion",
					ErrInvalidIngestionOutput, i, elementOf[p])
			default:
				item.ParentID = items[p].ID
			}
			children[p]++
		}
		itemOf[e.ElementID] = len(items)
		items = append(items, item)
		isGroup = append(isGroup, group)
		elementOf = append(elementOf, i)
		children = append(children, 0)
	}
	for k := range items {
		if isGroup[k] && children[k] == 0 {
			return nil, fmt.Errorf("%w: elements[%d] is a group with no child; an empty group would import as a control. Re-run the conversion",
				ErrInvalidIngestionOutput, elementOf[k])
		}
	}
	return items, nil
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
