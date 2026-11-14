package oscal_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

func FuzzCleanProse(f *testing.F) {
	// Seed corpus
	f.Add("The {{ insert: param, ac-2_prm_1 }} must comply.")
	f.Add("")
	f.Add("No templates here, just plain text.")
	f.Add("{{ insert: param, missing-id }}")
	f.Add("Multiple {{ insert: param, a }} and {{ insert: param, b }}.")
	f.Add("Unmatched }} brace is fine")

	f.Fuzz(func(t *testing.T, text string) {
		// Must not panic; params=nil is a valid call
		_ = oscal.CleanProse(text, nil)
	})
}

func FuzzCleanForEmbedding(f *testing.F) {
	// Seed corpus
	f.Add("  lots   of   spaces  ")
	f.Add("line1\n\n\n\n\nline2")
	f.Add("| col1 | col2 |\n|------|------|\ndata")
	f.Add("")
	f.Add("VerDate Sep 11 2014 some text")
	f.Add("G:\\COMP\\TITLE42.xml some path")

	f.Fuzz(func(t *testing.T, text string) {
		result := oscal.CleanForEmbedding(text)

		// Output must never have 3+ consecutive newlines
		if strings.Contains(result, "\n\n\n") {
			t.Errorf("output contains 3+ consecutive newlines: %q", result)
		}
	})
}

func FuzzDecomposeText(f *testing.F) {
	// Seed corpus: baseID, text, minWords
	f.Add("ac-2", "(a) First requirement\n(b) Second requirement", 1)
	f.Add("sc-1", "Simple text without structure.", 5)
	f.Add("", "", 1)
	f.Add("ctrl-1", "1. First item\n2. Second item\n3. Third item", 8)
	f.Add("au-3", "(a) The organization monitors all activity\n(b) The system logs events", 0)

	f.Fuzz(func(t *testing.T, baseID string, text string, minWords int) {
		// Clamp minWords to [1, 1000] to avoid degenerate cases
		if minWords < 1 {
			minWords = 1
		}
		if minWords > 1000 {
			minWords = 1000
		}

		// Must not panic
		items := oscal.DecomposeText(baseID, text, minWords)

		// Must always return at least one item
		if len(items) == 0 {
			t.Error("DecomposeText returned zero items")
		}

		// Every item must have non-nil Props
		for i, item := range items {
			if item.Props == nil {
				t.Errorf("item %d (%s) has nil Props", i, item.ID)
			}
		}
	})
}

func FuzzValidateSchema(f *testing.F) {
	// Seed corpus
	f.Add([]byte(`{"catalog": {}}`))
	f.Add([]byte(""))
	f.Add([]byte("not json at all"))
	f.Add([]byte(`{"key": "value"}`))
	f.Add([]byte(`[1, 2, 3]`))
	f.Add([]byte{0xFF, 0xFE})

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic. Use empty schemaPath (will fail gracefully).
		_ = oscal.ValidateSchema(data, "")
	})
}

func FuzzParse(f *testing.F) {
	const head = `{"catalog":{"uuid":"11111111-1111-4111-8111-111111111111","metadata":{"title":"t",` +
		`"last-modified":"2024-01-01T00:00:00Z","version":"1","oscal-version":"1.1.3"},`
	// Minimal valid catalog.
	f.Add([]byte(head + `"controls":[{"id":"c-1","title":"C","parts":[{"id":"c-1_smt","name":"statement","prose":"P."}]}]}}`))
	// Items nested six deep inside the statement.
	f.Add([]byte(head + `"controls":[{"id":"d","title":"D","parts":[{"id":"d_smt","name":"statement","prose":"L","parts":[` +
		`{"id":"d_smt.a","name":"item","parts":[{"id":"d_smt.a.1","name":"item","parts":[{"id":"d_smt.a.1.i","name":"item","parts":[` +
		`{"id":"d_smt.a.1.i.x","name":"item","parts":[{"id":"d_smt.a.1.i.x.y","name":"item","parts":[` +
		`{"id":"d_smt.a.1.i.x.y.z","name":"item","prose":"Leaf."}]}]}]}]}]}]}]}]}}`))
	// Unnamed and ID-less parts.
	f.Add([]byte(head + `"controls":[{"id":"u","title":"u","parts":[{"name":"statement","prose":"S","parts":[` +
		`{"prose":"no id or name"},{"name":"item","prose":"no id"},{"id":"other","prose":"no name"}]},{"prose":"top, unnamed"}]}]}}`))
	// Label props with class and namespace variants.
	f.Add([]byte(head + `"groups":[{"id":"g","title":"G","props":[{"name":"label","value":"G","ns":"http://example.com/ns"}],` +
		`"controls":[{"id":"l-1","title":"L-1","props":[{"name":"label","value":"L-01","class":"zero-padded"},` +
		`{"name":"label","value":"L-1","ns":"http://csrc.nist.gov/ns/oscal"}],"parts":[{"id":"l-1_ovw","name":"overview","prose":"O."}]}]}]}}`))
	// Invalid JSON.
	f.Add([]byte(`{"catalog":`))
	// Empty catalog.
	f.Add([]byte(`{"catalog":{}}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		tree, err := oscal.NewParser("").Parse(context.Background(), bytes.NewReader(data))
		if err != nil {
			return
		}
		seen := make(map[string]bool, len(tree.Items))
		for i, item := range tree.Items {
			if item.ID == "" {
				t.Fatalf("item %d has an empty ID", i)
			}
			if seen[item.ID] {
				t.Fatalf("item ID %q occurs more than once", item.ID)
			}
			if item.ParentID != "" && !seen[item.ParentID] {
				t.Fatalf("item %q names parent %q, which is not an earlier item", item.ID, item.ParentID)
			}
			seen[item.ID] = true
		}
		for _, tc := range tree.Textless {
			if !seen[tc.Identifier] {
				t.Fatalf("textless control %q is not an item", tc.Identifier)
			}
		}
	})
}
