package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/xuri/excelize/v2"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
)

// crosswalkRenderer writes a whole crosswalk in one output format.
type crosswalkRenderer func(w io.Writer, prov *pb.CrosswalkProvenance, rows []*pb.CrosswalkRow) error

// crosswalkFormats maps the --format values to their renderers.
var crosswalkFormats = map[string]crosswalkRenderer{
	"csv":  renderCSV,
	"json": renderJSON,
	"xlsx": renderXLSX,
	"md":   renderMarkdown,
}

// crosswalkColumns are the columns of every format, in order. They are the
// export's contract and differ from the proto field names; rowValues maps
// one to the other.
var crosswalkColumns = []string{
	"source_framework", "source_id", "source_parent", "source_title", "source_text",
	"relationship", "confidence", "direction", "hub_id",
	"target_framework", "target_id", "target_parent", "target_title", "target_text",
	"jobs_judged", "jobs_agreeing", "other_verdicts", "latest_job",
	"source_node_id", "target_node_id",
}

// crosswalkNumericColumns are written as JSON numbers; every other column
// is a string.
var crosswalkNumericColumns = map[string]bool{"confidence": true, "jobs_judged": true, "jobs_agreeing": true}

// rowValues returns r's values in crosswalkColumns order, unmodified:
// renderers neutralise them for their own format.
func rowValues(r *pb.CrosswalkRow) []string {
	s, t := r.GetSource(), r.GetTarget()
	return []string{
		s.GetFramework(), s.GetIdentifier(), s.GetParentIdentifier(), s.GetTitle(), s.GetStatement(),
		r.GetRelationship(), fmt.Sprintf("%.2f", r.GetConfidence()), r.GetDirection(), r.GetHubIdentifier(),
		t.GetFramework(), t.GetIdentifier(), t.GetParentIdentifier(), t.GetTitle(), t.GetStatement(),
		strconv.Itoa(int(r.GetJobsJudged())), strconv.Itoa(int(r.GetJobsAgreeing())), r.GetOtherVerdicts(), r.GetLatestJobId(),
		s.GetNodeId(), t.GetNodeId(),
	}
}

// renderCSV writes a UTF-8 byte order mark (so a spreadsheet reads the
// encoding), the header and one record per row. Control text is untrusted,
// so every cell is made safe as a spreadsheet cell (CWE-1236); text is kept
// whole, not cut to the display bound csvCell applies.
func renderCSV(w io.Writer, _ *pb.CrosswalkProvenance, rows []*pb.CrosswalkRow) error {
	if _, err := io.WriteString(w, "\xEF\xBB\xBF"); err != nil {
		return fmt.Errorf("write CSV byte order mark: %w", err)
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(crosswalkColumns); err != nil {
		return fmt.Errorf("write CSV header: %w", err)
	}
	for _, r := range rows {
		cells := rowValues(r)
		for i, c := range cells {
			cells[i] = csvCellUpTo(c, math.MaxInt)
		}
		if err := cw.Write(cells); err != nil {
			return fmt.Errorf("write CSV row: %w", err)
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("write CSV: %w", err)
	}
	return nil
}

type crosswalkCatalogJSON struct {
	CatalogID string `json:"catalog_id"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Imported  bool   `json:"imported"`
	Snapshot  bool   `json:"snapshot"`
}

// crosswalkProvenanceJSON records what produced the crosswalk, so a reader
// can reproduce it.
type crosswalkProvenanceJSON struct {
	Mode                   string                 `json:"mode"`
	Stage                  string                 `json:"stage"`
	Catalogs               []crosswalkCatalogJSON `json:"catalogs"`
	JobIDs                 []string               `json:"job_ids"`
	Rows                   int                    `json:"rows"`
	SkippedMissingControls int64                  `json:"skipped_missing_controls"`
	ServerVersion          string                 `json:"server_version"`
	GeneratedAt            string                 `json:"generated_at"`
}

// renderJSON writes {"provenance": {...}, "rows": [{<column>: value}]} with
// each row's keys in crosswalkColumns order. Rows is the number of rows
// written, which is the server's pre-pagination count because the CLI reads
// every page.
func renderJSON(w io.Writer, prov *pb.CrosswalkProvenance, rows []*pb.CrosswalkRow) error {
	stage := "synthesized"
	if prov.GetStage() == pb.CrosswalkStage_CROSSWALK_STAGE_ANALYZER {
		stage = "analyzer"
	}
	p := crosswalkProvenanceJSON{
		Mode:                   prov.GetMode(),
		Stage:                  stage,
		Catalogs:               make([]crosswalkCatalogJSON, 0, len(prov.GetCatalogs())),
		JobIDs:                 append([]string{}, prov.GetJobIds()...),
		Rows:                   len(rows),
		SkippedMissingControls: prov.GetSkippedMissingControls(),
		ServerVersion:          prov.GetServerVersion(),
	}
	if prov.GetGeneratedAt() != nil {
		p.GeneratedAt = prov.GetGeneratedAt().AsTime().UTC().Format(time.RFC3339)
	}
	for _, c := range prov.GetCatalogs() {
		p.Catalogs = append(p.Catalogs, crosswalkCatalogJSON{
			CatalogID: c.GetCatalogId(), Name: c.GetName(), Version: c.GetVersion(),
			Imported: c.GetImported(), Snapshot: c.GetSnapshot(),
		})
	}
	provJSON, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("encode crosswalk provenance as JSON: %w", err)
	}

	var buf bytes.Buffer
	buf.WriteString(`{"provenance":`)
	buf.Write(provJSON)
	buf.WriteString(`,"rows":[`)
	for i, r := range rows {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteByte('{')
		for j, v := range rowValues(r) {
			if j > 0 {
				buf.WriteByte(',')
			}
			key, _ := json.Marshal(crosswalkColumns[j])
			buf.Write(key)
			buf.WriteByte(':')
			if crosswalkNumericColumns[crosswalkColumns[j]] {
				buf.WriteString(v)
				continue
			}
			val, err := json.Marshal(v)
			if err != nil {
				return fmt.Errorf("encode crosswalk row %d as JSON: %w", i+1, err)
			}
			buf.Write(val)
		}
		buf.WriteByte('}')
	}
	buf.WriteString(`]}`)

	var out bytes.Buffer
	if err := json.Indent(&out, buf.Bytes(), "", "  "); err != nil {
		return fmt.Errorf("format crosswalk JSON: %w", err)
	}
	out.WriteByte('\n')
	if _, err := w.Write(out.Bytes()); err != nil {
		return fmt.Errorf("write crosswalk JSON: %w", err)
	}
	return nil
}

// markdownCellMaxRunes bounds a Markdown cell so one long control statement
// cannot make the table unreadable; CSV and JSON keep the whole text (after
// display cleaning for CSV) and XLSX keeps it up to a sheet cell's capacity.
const markdownCellMaxRunes = 200

// markdownEscaper HTML-escapes and then Markdown-escapes in one pass, so no
// replacement is itself re-escaped. Pipes would end the cell; the backslash
// set (\ [ ] ( ) ! * _ and the backtick) stops control text from forming
// links, images, emphasis or code.
var markdownEscaper = strings.NewReplacer(
	"&", "&amp;", "<", "&lt;", ">", "&gt;",
	`\`, `\\`, "[", `\[`, "]", `\]`, "(", `\(`, ")", `\)`, "!", `\!`, "*", `\*`, "_", `\_`, "`", "\\`",
	"|", `\|`,
)

// markdownCell makes untrusted text safe as one Markdown table cell for a
// renderer that allows HTML: the CSV/XLSX display cleaning (which also turns
// line breaks into spaces and drops terminal escapes and invisible or
// bidirectional characters), a cut to markdownCellMaxRunes runes ending in
// "…" (done on the raw text so an escape is never split), then escaping. No
// raw HTML survives, so the renderer emits none besides what it writes itself.
func markdownCell(s string) string {
	s = intanalyzer.CleanDisplay(s, math.MaxInt)
	if utf8.RuneCountInString(s) > markdownCellMaxRunes {
		s = string([]rune(s)[:markdownCellMaxRunes]) + "…"
	}
	return markdownEscaper.Replace(s)
}

// renderMarkdown writes a pipe table: the header, the separator and one line
// per row, in crosswalkColumns order. Provenance is not part of the table;
// use --format json or xlsx for it.
func renderMarkdown(w io.Writer, _ *pb.CrosswalkProvenance, rows []*pb.CrosswalkRow) error {
	var b strings.Builder
	line := func(cells []string) {
		b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
	}
	line(crosswalkColumns)
	separator := make([]string, len(crosswalkColumns))
	for i := range separator {
		separator[i] = "---"
	}
	line(separator)
	for _, r := range rows {
		cells := rowValues(r)
		for i, c := range cells {
			cells[i] = markdownCell(c)
		}
		line(cells)
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("write crosswalk Markdown: %w", err)
	}
	return nil
}

// xlsxCellText makes untrusted text safe as a spreadsheet cell and, when it
// exceeds the cell capacity (excelize.TotalCellChars UTF-16 units), cuts it to
// fit and ends it with "…" so the reader can see it was cut.
func xlsxCellText(s string) string {
	s = csvCellUpTo(s, math.MaxInt)
	if len(utf16.Encode([]rune(s))) <= excelize.TotalCellChars {
		return s
	}
	var b strings.Builder
	units := 0
	for _, r := range s {
		n := utf16.RuneLen(r)
		if units+n > excelize.TotalCellChars-1 {
			break
		}
		b.WriteRune(r)
		units += n
	}
	return b.String() + "…"
}

// writeXLSXTable fills sheet with a header row and rows of string cells,
// each made safe as a spreadsheet cell (CWE-1236) and written with
// SetCellStr, so none is a formula or a number. A cell holds at most
// excelize.TotalCellChars UTF-16 units; longer text is cut by xlsxCellText with
// a trailing "…" instead of silently by excelize. It freezes the header and
// adds an autofilter over the whole table.
func writeXLSXTable(f *excelize.File, sheet string, header []string, rows [][]string) error {
	for i, line := range append([][]string{header}, rows...) {
		for j, v := range line {
			cell, err := excelize.CoordinatesToCellName(j+1, i+1)
			if err != nil {
				return fmt.Errorf("name cell (%d,%d) of sheet %s: %w", j+1, i+1, sheet, err)
			}
			if err := f.SetCellStr(sheet, cell, xlsxCellText(v)); err != nil {
				return fmt.Errorf("write cell %s of sheet %s: %w", cell, sheet, err)
			}
		}
	}
	err := f.SetPanes(sheet, &excelize.Panes{
		Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft",
		Selection: []excelize.Selection{{SQRef: "A2", ActiveCell: "A2", Pane: "bottomLeft"}},
	})
	if err != nil {
		return fmt.Errorf("freeze header of sheet %s: %w", sheet, err)
	}
	last, err := excelize.CoordinatesToCellName(len(header), len(rows)+1)
	if err != nil {
		return fmt.Errorf("name filter range of sheet %s: %w", sheet, err)
	}
	if err := f.AutoFilter(sheet, "A1:"+last, nil); err != nil {
		return fmt.Errorf("add autofilter to sheet %s: %w", sheet, err)
	}
	return nil
}

// renderXLSX writes a workbook of three sheets: crosswalk (every row),
// provenance (what produced it, as field/value rows) and disagreements (the
// rows whose jobs_agreeing is below jobs_judged, for a reviewer's attention).
func renderXLSX(w io.Writer, prov *pb.CrosswalkProvenance, rows []*pb.CrosswalkRow) error {
	f := excelize.NewFile()
	defer f.Close()

	all := make([][]string, 0, len(rows))
	var disagreements [][]string
	for _, r := range rows {
		v := rowValues(r)
		all = append(all, v)
		if r.GetJobsAgreeing() < r.GetJobsJudged() {
			disagreements = append(disagreements, v)
		}
	}

	stage := "synthesized"
	if prov.GetStage() == pb.CrosswalkStage_CROSSWALK_STAGE_ANALYZER {
		stage = "analyzer"
	}
	generatedAt := ""
	if prov.GetGeneratedAt() != nil {
		generatedAt = prov.GetGeneratedAt().AsTime().UTC().Format(time.RFC3339)
	}
	provenance := [][]string{
		{"mode", prov.GetMode()},
		{"stage", stage},
		{"rows", strconv.Itoa(len(rows))},
		{"skipped_missing_controls", strconv.FormatInt(prov.GetSkippedMissingControls(), 10)},
		{"server_version", prov.GetServerVersion()},
		{"generated_at", generatedAt},
	}
	for _, c := range prov.GetCatalogs() {
		provenance = append(provenance, []string{"catalog", fmt.Sprintf("%s | %s | %s | imported=%t | snapshot=%t",
			c.GetCatalogId(), c.GetName(), c.GetVersion(), c.GetImported(), c.GetSnapshot())})
	}
	for _, id := range prov.GetJobIds() {
		provenance = append(provenance, []string{"job_id", id})
	}

	if err := f.SetSheetName(f.GetSheetName(0), "crosswalk"); err != nil {
		return fmt.Errorf("name crosswalk sheet: %w", err)
	}
	for _, s := range []struct {
		name   string
		header []string
		rows   [][]string
	}{
		{"crosswalk", crosswalkColumns, all},
		{"provenance", []string{"field", "value"}, provenance},
		{"disagreements", crosswalkColumns, disagreements},
	} {
		if s.name != "crosswalk" {
			if _, err := f.NewSheet(s.name); err != nil {
				return fmt.Errorf("add sheet %s: %w", s.name, err)
			}
		}
		if err := writeXLSXTable(f, s.name, s.header, s.rows); err != nil {
			return err
		}
	}
	if err := f.Write(w); err != nil {
		return fmt.Errorf("write crosswalk XLSX: %w", err)
	}
	return nil
}
