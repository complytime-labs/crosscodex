package main

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
)

// fuzzCrosswalkSeeds are the control titles both targets start from: a
// formula, a pipe, line breaks, a long text, unicode and quotes.
var fuzzCrosswalkSeeds = []string{
	`=HYPERLINK("http://x")`,
	"a | b | c",
	"line one\nline two\r\nline three\rfour",
	strings.Repeat("long ", 100),
	"héllo 世界 \u202e end \u2028 next",
	`say "hi", then, leave`,
	"@SUM(A1)\t+cmd|' /C calc'!A0",
	"a <!-- x --> b <img src=x> &lt; [click](http://evil) ![i](u)",
	"\x1b[31mred\u202e\u200b",
}

func fuzzCrosswalkRows(title string) []*pb.CrosswalkRow {
	side := &pb.CrosswalkSide{Framework: "F", Identifier: "id", Context: title, Title: title, Statement: title}
	return []*pb.CrosswalkRow{
		{Source: side, Target: side, Relationship: "equivalent", Confidence: 0.5, Direction: "direct", Review: title, JobsJudged: 1, JobsAgreeing: 1},
		{Source: side, Target: side, Relationship: "subset of", Confidence: 0.25, Direction: "direct", JobsJudged: 2, JobsAgreeing: 1},
	}
}

// FuzzRenderCSV checks that any control text leaves the CSV parseable as the
// header plus one record per row, each with every column.
func FuzzRenderCSV(f *testing.F) {
	for _, seed := range fuzzCrosswalkSeeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, title string) {
		rows := fuzzCrosswalkRows(title)
		var buf bytes.Buffer
		if err := renderCSV(&buf, nil, rows); err != nil {
			t.Fatalf("renderCSV: %v", err)
		}
		records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(buf.String(), "\xEF\xBB\xBF"))).ReadAll()
		if err != nil {
			t.Fatalf("output is not valid CSV: %v", err)
		}
		if len(records) != len(rows)+1 {
			t.Fatalf("got %d records, want %d", len(records), len(rows)+1)
		}
		for i, rec := range records {
			if len(rec) != len(crosswalkColumns) {
				t.Fatalf("record %d has %d fields, want %d", i, len(rec), len(crosswalkColumns))
			}
		}
	})
}

// FuzzRenderMarkdown checks that any control text leaves the table as the
// header, the separator and exactly one line per row, with no unescaped pipe
// inside a cell and no HTML tag opened by control text.
func FuzzRenderMarkdown(f *testing.F) {
	for _, seed := range fuzzCrosswalkSeeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, title string) {
		rows := fuzzCrosswalkRows(title)
		var buf bytes.Buffer
		if err := renderMarkdown(&buf, nil, rows); err != nil {
			t.Fatalf("renderMarkdown: %v", err)
		}
		out := buf.String()
		if !strings.HasSuffix(out, "\n") {
			t.Fatalf("output does not end in a newline: %q", out)
		}
		lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
		if len(lines) != len(rows)+2 {
			t.Fatalf("got %d lines, want %d: %q", len(lines), len(rows)+2, out)
		}
		if strings.Contains(out, "<") {
			t.Fatalf("output contains a raw '<': %q", out)
		}
		for i, line := range lines {
			// Cells are joined by " | " and escaped pipes are "\|": removing
			// the escapes leaves exactly one pipe per column boundary.
			if n := strings.Count(strings.ReplaceAll(line, `\|`, ""), "|"); n != len(crosswalkColumns)+1 {
				t.Fatalf("line %d has %d column pipes, want %d: %q", i, n, len(crosswalkColumns)+1, line)
			}
		}
	})
}
