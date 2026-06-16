package oscal_test

import (
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
