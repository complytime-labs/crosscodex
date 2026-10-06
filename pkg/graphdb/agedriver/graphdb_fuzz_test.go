package agedriver_test

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/agedriver"
)

func FuzzEscapeCypher(f *testing.F) {
	f.Add("hello world")
	f.Add("")
	f.Add("it's a test")
	f.Add(`back\slash`)
	f.Add("'; DROP TABLE users; --")
	f.Add(`\'\\'`)
	f.Add("unicode: ￿")
	f.Add("inject" + agedriver.ExportCypherDollarTag + "payload")
	f.Add("$cyp" + agedriver.ExportCypherDollarTag + "her$")
	f.Add("$$")

	f.Fuzz(func(t *testing.T, input string) {
		result := agedriver.EscapeCypher(input)

		// Every single quote in the output must be preceded by a backslash.
		for i := 0; i < len(result); i++ {
			if result[i] == '\'' {
				if i == 0 || result[i-1] != '\\' {
					t.Errorf("unescaped single quote at position %d in %q (input: %q)", i, result, input)
				}
			}
		}

		// The dollar-quote tag must never appear in escaped output.
		if strings.Contains(result, agedriver.ExportCypherDollarTag) {
			t.Errorf("dollar-quote tag %q found in output %q (input: %q)",
				agedriver.ExportCypherDollarTag, result, input)
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
		_, _ = agedriver.ParseAGVertex(input)
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
		_, _ = agedriver.ParseAGEdge(input)
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
		_, _ = agedriver.ParseAGPath(input)
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
		_ = agedriver.SplitAGPathElements(input)
	})
}

// validParamKey matches the identifier shape substituteParams accepts for a
// map key: the same character class as paramToken, minus the leading `$`.
var validParamKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// wholeTokenScanner independently identifies `$name` tokens of the same
// shape substituteParams recognizes (see paramToken in cypher.go), without
// depending on that unexported regex. It drives the oracle in
// FuzzSubstituteParams: scanning the original cypher and replacing only the
// tokens that equal "$"+key is a position-aware way to compute the expected
// output, immune to the inserted literal accidentally forming new
// token-shaped text together with adjacent, unrelated characters.
var wholeTokenScanner = regexp.MustCompile(`\$[A-Za-z_][A-Za-z0-9_]*`)

// FuzzSubstituteParams checks that substitution never panics, is the
// identity without parameters, leaves the query unchanged whenever the key
// cannot possibly match a `$name` token (non-identifier shape) or the query
// contains no whole `$key` token, and — when a whole `$key` token is present
// — matches wholeTokenScanner's independent, position-aware oracle: replace
// every token equal to "$"+key with "'"+escapeCypher(value)+"'" and copy
// everything else verbatim. The oracle matters because string-stripping the
// inserted literal from the output is unsound: see the seed with cypher
// "$a" + two single quotes + "b, $ab", key "ab", value "" — removing every
// occurrence of two-single-quotes from the (correct) output joins the
// unrelated leftover "$a" and "b" into a spurious "$ab" that looks like a
// leftover token but is not one. Dollar-quote safety of the inserted
// literal belongs to escapeCypher and FuzzEscapeCypher, not to this
// function.
func FuzzSubstituteParams(f *testing.F) {
	f.Add("MATCH (n {id: $id}) RETURN n", "id", "a")
	f.Add("RETURN $id2, $id", "id", "x")
	f.Add("", "id", "a")
	f.Add("$", "", "")
	f.Add("RETURN $v", "v", "'; MATCH (n) DETACH DELETE n; //")
	f.Add("RETURN $v", "v", agedriver.ExportCypherDollarTag)
	f.Add("RETURN $v", "v", "$v")
	f.Add("$a''b, $ab", "ab", "")

	f.Fuzz(func(t *testing.T, cypher, key, value string) {
		if got := agedriver.SubstituteParams(cypher, nil); got != cypher {
			t.Fatalf("substituteParams(%q, nil) = %q; want unchanged", cypher, got)
		}
		got := agedriver.SubstituteParams(cypher, map[string]string{key: value})

		if !validParamKey.MatchString(key) {
			if got != cypher {
				t.Fatalf("query with non-identifier key %q changed: %q -> %q", key, cypher, got)
			}
			return
		}

		wholeToken := regexp.MustCompile(`\$` + regexp.QuoteMeta(key) + `(?:[^A-Za-z0-9_]|$)`)
		if !wholeToken.MatchString(cypher) {
			if got != cypher {
				t.Fatalf("query without whole token $%s changed: %q -> %q", key, cypher, got)
			}
			return
		}

		// The query contains a whole $key token: substitution must have
		// inserted the escaped value as a quoted Cypher string literal.
		quoted := "'" + agedriver.EscapeCypher(value) + "'"
		if !strings.Contains(got, quoted) {
			t.Fatalf("query with whole token $%s did not contain escaped literal %q: got %q (cypher: %q, value: %q)",
				key, quoted, got, cypher, value)
		}

		// Position-aware oracle: scan for whole $name tokens independently
		// of substituteParams' own regex, replace every token equal to
		// "$"+key with the quoted literal, and copy everything else
		// verbatim.
		want := wholeTokenScanner.ReplaceAllStringFunc(cypher, func(tok string) string {
			if tok[1:] != key {
				return tok
			}
			return quoted
		})
		if got != want {
			t.Fatalf("substituteParams(%q, {%q: %q}) = %q; want %q (independent token scan)",
				cypher, key, value, got, want)
		}
	})
}

// unescapedQuotes counts single quotes not escaped by an odd run of
// backslashes, i.e. the quotes that open or close a Cypher string literal.
func unescapedQuotes(s string) int {
	n, backslashes := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			backslashes++
			continue
		case '\'':
			if backslashes%2 == 0 {
				n++
			}
		}
		backslashes = 0
	}
	return n
}

func FuzzCypherStringList(f *testing.F) {
	f.Add("a", "b")
	f.Add("", "")
	f.Add("o'brien", `back\slash`)
	f.Add(`\'`, "', 'x")
	f.Add("inject"+agedriver.ExportCypherDollarTag+"payload", "$cyp"+agedriver.ExportCypherDollarTag+"her$")

	f.Fuzz(func(t *testing.T, a, b string) {
		out := agedriver.CypherStringList([]string{a, b})
		if strings.Contains(out, agedriver.ExportCypherDollarTag) {
			t.Errorf("dollar-quote tag in %q (inputs %q, %q)", out, a, b)
		}
		// Two literals, so exactly four delimiting quotes: nothing broke out.
		if got := unescapedQuotes(out); got != 4 {
			t.Errorf("%d unescaped quotes in %q, want 4 (inputs %q, %q)", got, out, a, b)
		}
	})
}

func FuzzCreateLabelEdges(f *testing.F) {
	f.Add("src", "tgt", "e1", "note")
	f.Add("", "", "", "")
	f.Add("o'brien", `back\slash`, `\'`, "'}]->(x) DETACH DELETE x //")
	f.Add("inject"+agedriver.ExportCypherDollarTag+"payload", "$cyp"+agedriver.ExportCypherDollarTag+"her$", "$$", "$cypher")

	f.Fuzz(func(t *testing.T, src, tgt, id, note string) {
		out := agedriver.CreateLabelEdges("MAPS", []graphdb.BulkEdge{{
			SourceID: src, TargetID: tgt,
			Edge: graphdb.Edge{ID: id, Label: "MAPS", ValidFrom: time.Unix(0, 0), Properties: map[string]any{"note": note}},
		}})
		if strings.Contains(out, agedriver.ExportCypherDollarTag) {
			t.Errorf("dollar-quote tag in %q", out)
		}
		// Five value literals (s, t, id, valid_from, note) and three key
		// subscripts (id, valid_from, note), two quotes each.
		if got := unescapedQuotes(out); got != 16 {
			t.Errorf("%d unescaped quotes in %q, want 16", got, out)
		}
	})
}
