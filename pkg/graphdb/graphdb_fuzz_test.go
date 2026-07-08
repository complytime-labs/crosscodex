package graphdb_test

import (
	"strings"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

func FuzzEscapeCypher(f *testing.F) {
	f.Add("hello world")
	f.Add("")
	f.Add("it's a test")
	f.Add(`back\slash`)
	f.Add("'; DROP TABLE users; --")
	f.Add(`\'\\'`)
	f.Add("unicode: \u0000\uffff")
	f.Add("inject" + graphdb.ExportCypherDollarTag + "payload")
	f.Add("$$")

	f.Fuzz(func(t *testing.T, input string) {
		result := graphdb.EscapeCypher(input)

		// Every single quote in the output must be preceded by a backslash.
		for i := 0; i < len(result); i++ {
			if result[i] == '\'' {
				if i == 0 || result[i-1] != '\\' {
					t.Errorf("unescaped single quote at position %d in %q (input: %q)", i, result, input)
				}
			}
		}

		// The dollar-quote tag must never appear in escaped output.
		if strings.Contains(result, graphdb.ExportCypherDollarTag) {
			t.Errorf("dollar-quote tag %q found in output %q (input: %q)",
				graphdb.ExportCypherDollarTag, result, input)
		}
	})
}

func FuzzParseAGVertex(f *testing.F) {
	f.Add(`{"id": 1, "label": "Node", "properties": {"id": "n-1", "valid_from": "2025-01-01T00:00:00Z"}}::vertex`)
	f.Add("")
	f.Add(`not json at all`)
	f.Add(`{"id": 1}::vertex`)
	f.Add(`{}::vertex`)
	f.Add(`{"id": 1, "label": "", "properties": {}}::vertex`)

	f.Fuzz(func(t *testing.T, input string) {
		// Must not panic regardless of input.
		_, _ = graphdb.ParseAGVertex(input)
	})
}

func FuzzParseAGEdge(f *testing.F) {
	f.Add(`{"id": 1, "label": "REL", "start_id": 1, "end_id": 2, "properties": {"id": "e-1", "source": "a", "target": "b", "valid_from": "2025-01-01T00:00:00Z"}}::edge`)
	f.Add("")
	f.Add(`garbage data`)
	f.Add(`{"id": 1}::edge`)
	f.Add(`{}::edge`)
	f.Add(`{"id": 1, "label": "", "start_id": 0, "end_id": 0, "properties": {}}::edge`)

	f.Fuzz(func(t *testing.T, input string) {
		// Must not panic regardless of input.
		_, _ = graphdb.ParseAGEdge(input)
	})
}

func FuzzParseAGPath(f *testing.F) {
	vertex := `{"id": 1, "label": "A", "properties": {"id": "n-1", "valid_from": "2025-01-01T00:00:00Z"}}::vertex`
	edge := `{"id": 10, "label": "R", "start_id": 1, "end_id": 2, "properties": {"id": "e-1", "source": "n-1", "target": "n-2", "valid_from": "2025-01-01T00:00:00Z"}}::edge`
	vertex2 := `{"id": 2, "label": "B", "properties": {"id": "n-2", "valid_from": "2025-01-01T00:00:00Z"}}::vertex`
	validPath := "[" + vertex + ", " + edge + ", " + vertex2 + "]::path"

	f.Add(validPath)
	f.Add("")
	f.Add("[" + vertex + ", " + edge + ", " + vertex2 + ", " + edge + ", " + vertex + "]::path")
	f.Add("[]::path")
	f.Add(`{"id": 1}::path`)

	f.Fuzz(func(t *testing.T, input string) {
		// Must not panic regardless of input.
		_, _ = graphdb.ParseAGPath(input)
	})
}

func FuzzSplitAGPathElements(f *testing.F) {
	f.Add(`{"id": 1, "label": "A", "properties": {}}::vertex, {"id": 10, "label": "R", "start_id": 1, "end_id": 2, "properties": {}}::edge`)
	f.Add("")
	f.Add(`{{{`)
	f.Add(`{"a": 1}, {"b": 2}`)
	f.Add(`no braces at all`)
	f.Add(`{"deeply": {"nested": {"value": 1}}}::vertex`)

	f.Fuzz(func(t *testing.T, input string) {
		// Must not panic regardless of input.
		_ = graphdb.SplitAGPathElements(input)
	})
}
