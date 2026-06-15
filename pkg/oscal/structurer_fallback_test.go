package oscal

import (
	"testing"
)

func TestTierFallback(t *testing.T) {
	t.Run("splits by double newline (paragraph breaks)", func(t *testing.T) {
		doc := StructuredDoc{
			RawText: `First paragraph with some text.

Second paragraph with more content.

Third paragraph here.`,
		}
		opts := StructureOptions{}

		items, ok := TierFallback(doc, opts)
		if !ok {
			t.Fatal("expected true (fallback always succeeds)")
		}
		if len(items) != 3 {
			t.Fatalf("expected 3 items, got %d", len(items))
		}

		expected := []struct {
			id   string
			text string
		}{
			{"para-1", "First paragraph with some text."},
			{"para-2", "Second paragraph with more content."},
			{"para-3", "Third paragraph here."},
		}

		for i, exp := range expected {
			if items[i].ID != exp.id {
				t.Errorf("item %d: expected ID %q, got %q", i, exp.id, items[i].ID)
			}
			if items[i].Text != exp.text {
				t.Errorf("item %d: expected text %q, got %q", i, exp.text, items[i].Text)
			}
			if items[i].Class != ClassRequirement {
				t.Errorf("item %d: expected Class %q, got %q", i, ClassRequirement, items[i].Class)
			}
		}
	})

	t.Run("skips empty paragraphs", func(t *testing.T) {
		doc := StructuredDoc{
			RawText: `First paragraph.


Second paragraph.

`,
		}
		opts := StructureOptions{}

		items, ok := TierFallback(doc, opts)
		if !ok {
			t.Fatal("expected true")
		}
		// Should only get 2 items (empty paragraphs skipped)
		if len(items) != 2 {
			t.Fatalf("expected 2 items (empty paragraphs skipped), got %d", len(items))
		}
		if items[0].Text != "First paragraph." {
			t.Errorf("expected text %q, got %q", "First paragraph.", items[0].Text)
		}
		if items[1].Text != "Second paragraph." {
			t.Errorf("expected text %q, got %q", "Second paragraph.", items[1].Text)
		}
	})

	t.Run("handles single paragraph (no double newlines)", func(t *testing.T) {
		doc := StructuredDoc{
			RawText: "Just one paragraph with no breaks.",
		}
		opts := StructureOptions{}

		items, ok := TierFallback(doc, opts)
		if !ok {
			t.Fatal("expected true")
		}
		if len(items) != 1 {
			t.Fatalf("expected 1 item, got %d", len(items))
		}
		if items[0].ID != "para-1" {
			t.Errorf("expected ID para-1, got %q", items[0].ID)
		}
		if items[0].Text != "Just one paragraph with no breaks." {
			t.Errorf("unexpected text: %q", items[0].Text)
		}
	})

	t.Run("always returns true even with empty text", func(t *testing.T) {
		doc := StructuredDoc{RawText: ""}
		opts := StructureOptions{}

		items, ok := TierFallback(doc, opts)
		if !ok {
			t.Error("expected true (fallback always succeeds)")
		}
		// Empty text means no paragraphs, but still returns true
		if len(items) != 0 {
			t.Errorf("expected 0 items for empty text, got %d", len(items))
		}
	})

	t.Run("always returns true with only whitespace", func(t *testing.T) {
		doc := StructuredDoc{RawText: "\n\n\n   \n\n"}
		opts := StructureOptions{}

		items, ok := TierFallback(doc, opts)
		if !ok {
			t.Error("expected true (fallback always succeeds)")
		}
		// Whitespace-only paragraphs are skipped
		if len(items) != 0 {
			t.Errorf("expected 0 items for whitespace-only text, got %d", len(items))
		}
	})

	t.Run("trims whitespace from paragraphs", func(t *testing.T) {
		doc := StructuredDoc{
			RawText: `  First with leading/trailing spaces

  Second paragraph  `,
		}
		opts := StructureOptions{}

		items, ok := TierFallback(doc, opts)
		if !ok {
			t.Fatal("expected true")
		}
		if len(items) != 2 {
			t.Fatalf("expected 2 items, got %d", len(items))
		}
		if items[0].Text != "First with leading/trailing spaces" {
			t.Errorf("expected trimmed text, got %q", items[0].Text)
		}
		if items[1].Text != "Second paragraph" {
			t.Errorf("expected trimmed text, got %q", items[1].Text)
		}
	})

	t.Run("handles multiple consecutive empty paragraphs", func(t *testing.T) {
		doc := StructuredDoc{
			RawText: `Content



Another content`,
		}
		opts := StructureOptions{}

		items, ok := TierFallback(doc, opts)
		if !ok {
			t.Fatal("expected true")
		}
		if len(items) != 2 {
			t.Fatalf("expected 2 items (empty para skipped), got %d", len(items))
		}
	})
}
