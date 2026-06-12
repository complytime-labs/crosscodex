package oscal

import (
	"testing"
)

func TestTierHeading(t *testing.T) {
	t.Run("returns false when no sections", func(t *testing.T) {
		doc := StructuredDoc{Sections: []DocSection{}}
		opts := StructureOptions{}

		items, ok := TierHeading(doc, opts)
		if ok {
			t.Error("expected false when no sections present")
		}
		if items != nil {
			t.Error("expected nil items when no sections present")
		}
	})

	t.Run("extracts sections with unique titles", func(t *testing.T) {
		doc := StructuredDoc{
			Sections: []DocSection{
				{Level: 1, Title: "Introduction", Text: "This is the intro"},
				{Level: 2, Title: "Requirements", Text: "These are requirements"},
				{Level: 2, Title: "Compliance", Text: "Compliance rules"},
			},
		}
		opts := StructureOptions{}

		items, ok := TierHeading(doc, opts)
		if !ok {
			t.Fatal("expected true when sections found")
		}
		if len(items) != 3 {
			t.Fatalf("expected 3 items, got %d", len(items))
		}

		expected := []struct {
			id    string
			title string
			text  string
		}{
			{"sec-1", "Introduction", "This is the intro"},
			{"sec-2", "Requirements", "These are requirements"},
			{"sec-3", "Compliance", "Compliance rules"},
		}

		for i, exp := range expected {
			if items[i].ID != exp.id {
				t.Errorf("item %d: expected ID %q, got %q", i, exp.id, items[i].ID)
			}
			if items[i].Title != exp.title {
				t.Errorf("item %d: expected title %q, got %q", i, exp.title, items[i].Title)
			}
			if items[i].Text != exp.text {
				t.Errorf("item %d: expected text %q, got %q", i, exp.text, items[i].Text)
			}
			if items[i].Class != ClassRequirement {
				t.Errorf("item %d: expected Class %q, got %q", i, ClassRequirement, items[i].Class)
			}
		}
	})

	t.Run("filters out headings that repeat more than MaxHeadingRepeats times (default 3)", func(t *testing.T) {
		doc := StructuredDoc{
			Sections: []DocSection{
				{Level: 1, Title: "Spurious", Text: "First"},
				{Level: 1, Title: "Spurious", Text: "Second"},
				{Level: 1, Title: "Spurious", Text: "Third"},
				{Level: 1, Title: "Spurious", Text: "Fourth"}, // This makes it 4 occurrences
				{Level: 1, Title: "Real Heading", Text: "Real content"},
			},
		}
		opts := StructureOptions{} // MaxHeadingRepeats defaults to 3

		items, ok := TierHeading(doc, opts)
		if !ok {
			t.Fatal("expected true when valid sections remain")
		}
		// Only "Real Heading" should remain; "Spurious" appears 4 times (> 3)
		if len(items) != 1 {
			t.Fatalf("expected 1 item after filtering, got %d", len(items))
		}
		if items[0].Title != "Real Heading" {
			t.Errorf("expected title %q, got %q", "Real Heading", items[0].Title)
		}
	})

	t.Run("respects custom MaxHeadingRepeats", func(t *testing.T) {
		doc := StructuredDoc{
			Sections: []DocSection{
				{Level: 1, Title: "Common", Text: "First"},
				{Level: 1, Title: "Common", Text: "Second"},
				{Level: 1, Title: "Unique", Text: "Content"},
			},
		}
		opts := StructureOptions{MaxHeadingRepeats: 1} // Anything appearing > 1 time is filtered

		items, ok := TierHeading(doc, opts)
		if !ok {
			t.Fatal("expected true when valid sections remain")
		}
		// Only "Unique" should remain; "Common" appears 2 times (> 1)
		if len(items) != 1 {
			t.Fatalf("expected 1 item, got %d", len(items))
		}
		if items[0].Title != "Unique" {
			t.Errorf("expected title %q, got %q", "Unique", items[0].Title)
		}
	})

	t.Run("returns false when all sections are filtered out", func(t *testing.T) {
		doc := StructuredDoc{
			Sections: []DocSection{
				{Level: 1, Title: "Same", Text: "A"},
				{Level: 1, Title: "Same", Text: "B"},
				{Level: 1, Title: "Same", Text: "C"},
				{Level: 1, Title: "Same", Text: "D"}, // 4 occurrences
			},
		}
		opts := StructureOptions{MaxHeadingRepeats: 3}

		items, ok := TierHeading(doc, opts)
		if ok {
			t.Error("expected false when all sections filtered out")
		}
		if items != nil {
			t.Error("expected nil items when all sections filtered out")
		}
	})
}
