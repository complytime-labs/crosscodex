package oscal

import (
	"context"
	"testing"
)

func TestStructurer_FallsThroughTiersToFindMatch(t *testing.T) {
	// Create a document with sections → should match TierHeading
	doc := StructuredDoc{
		Sections: []DocSection{
			{Level: 1, Title: "Introduction", Text: "This is the introduction."},
			{Level: 1, Title: "Requirements", Text: "The system shall implement authentication."},
		},
		RawText: "Introduction\n\nThis is the introduction.\n\nRequirements\n\nThe system shall implement authentication.",
	}

	structurer := NewStructurer(nil, nil)
	opts := StructureOptions{}

	items, err := structurer.Structure(context.Background(), doc, opts)
	if err != nil {
		t.Fatalf("Structure() failed: %v", err)
	}

	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}

	if items[0].Title != "Introduction" {
		t.Errorf("expected first item title to be 'Introduction', got %q", items[0].Title)
	}

	if items[1].Title != "Requirements" {
		t.Errorf("expected second item title to be 'Requirements', got %q", items[1].Title)
	}
}

func TestStructurer_FallsToParagraphFallback(t *testing.T) {
	// Create a document with only plain text (no sections, no tables, no patterns)
	doc := StructuredDoc{
		RawText: "This is the first paragraph.\n\nThis is the second paragraph.\n\nThis is the third paragraph.",
	}

	structurer := NewStructurer(nil, nil)
	opts := StructureOptions{}

	items, err := structurer.Structure(context.Background(), doc, opts)
	if err != nil {
		t.Fatalf("Structure() failed: %v", err)
	}

	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}

	if items[0].ID != "para-1" {
		t.Errorf("expected first item ID to be 'para-1', got %q", items[0].ID)
	}

	if items[1].ID != "para-2" {
		t.Errorf("expected second item ID to be 'para-2', got %q", items[1].ID)
	}

	if items[2].ID != "para-3" {
		t.Errorf("expected third item ID to be 'para-3', got %q", items[2].ID)
	}
}

func TestStructurer_AppliesKeywordFiltering(t *testing.T) {
	// Create a document with mixed content (some with keywords, some without)
	doc := StructuredDoc{
		Sections: []DocSection{
			{Level: 1, Title: "Background", Text: "This is background information."},
			{Level: 1, Title: "Requirements", Text: "The system shall implement authentication."},
			{Level: 1, Title: "Overview", Text: "This is an overview."},
			{Level: 1, Title: "Compliance", Text: "All systems must comply with these requirements."},
		},
		RawText: "Background\n\nThis is background information.\n\nRequirements\n\nThe system shall implement authentication.\n\nOverview\n\nThis is an overview.\n\nCompliance\n\nAll systems must comply with these requirements.",
	}

	structurer := NewStructurer(nil, nil)
	opts := StructureOptions{
		FilterByKeywords: true,
		Keywords:         []string{"shall", "must"},
	}

	items, err := structurer.Structure(context.Background(), doc, opts)
	if err != nil {
		t.Fatalf("Structure() failed: %v", err)
	}

	// Should only keep items with "shall" or "must"
	if len(items) != 2 {
		t.Fatalf("expected 2 items after filtering, got %d", len(items))
	}

	// Check that filtered items contain keywords
	for _, item := range items {
		if item.Title != "Requirements" && item.Title != "Compliance" {
			t.Errorf("unexpected item after filtering: %q", item.Title)
		}
	}
}

func TestStructurer_AppliesDecomposition(t *testing.T) {
	// Create a document with parenthesized clauses that should be decomposed
	doc := StructuredDoc{
		Sections: []DocSection{
			{
				Level: 1,
				Title: "Access Control",
				Text:  "The system shall:\n(a) implement authentication mechanisms\n(b) implement authorization controls\n(c) implement audit logging",
			},
		},
		RawText: "Access Control\n\nThe system shall:\n(a) implement authentication mechanisms\n(b) implement authorization controls\n(c) implement audit logging",
	}

	structurer := NewStructurer(nil, nil)
	opts := StructureOptions{
		Decompose:         true,
		MinDecomposeWords: 5, // Lower threshold for testing
	}

	items, err := structurer.Structure(context.Background(), doc, opts)
	if err != nil {
		t.Fatalf("Structure() failed: %v", err)
	}

	// After decomposition, we should have more items
	// The original text contains 3 parenthesized clauses, so expect at least 3 items
	if len(items) < 3 {
		t.Fatalf("expected at least 3 items after decomposition, got %d", len(items))
	}

	// Check that we have items with .a, .b, .c IDs
	foundA, foundB, foundC := false, false, false
	for _, item := range items {
		if item.ID == "sec-1.a" {
			foundA = true
		}
		if item.ID == "sec-1.b" {
			foundB = true
		}
		if item.ID == "sec-1.c" {
			foundC = true
		}
	}

	if !foundA || !foundB || !foundC {
		t.Errorf("expected to find decomposed items with IDs .a, .b, .c")
		for _, item := range items {
			t.Logf("  %s: %s", item.ID, item.Text)
		}
	}
}

func TestStructurer_ReturnsErrorWhenAllTiersFail(t *testing.T) {
	// Create a document that should fail all tiers
	// Empty RawText and no sections/tables should fail TierFallback
	doc := StructuredDoc{
		RawText: "",
	}

	structurer := NewStructurer(nil, nil)
	opts := StructureOptions{}

	items, err := structurer.Structure(context.Background(), doc, opts)
	if err != ErrStructureFailed {
		t.Fatalf("expected ErrStructureFailed, got %v with %d items", err, len(items))
	}
}

func TestStructurer_KeywordFilteringFallback(t *testing.T) {
	// Test that if all items are filtered out, we keep the original set
	doc := StructuredDoc{
		Sections: []DocSection{
			{Level: 1, Title: "Background", Text: "This is background information."},
			{Level: 1, Title: "Overview", Text: "This is an overview."},
		},
		RawText: "Background\n\nThis is background information.\n\nOverview\n\nThis is an overview.",
	}

	structurer := NewStructurer(nil, nil)
	opts := StructureOptions{
		FilterByKeywords: true,
		Keywords:         []string{"shall", "must"}, // Keywords that don't appear in the text
	}

	items, err := structurer.Structure(context.Background(), doc, opts)
	if err != nil {
		t.Fatalf("Structure() failed: %v", err)
	}

	// Should keep original set since all items were filtered out
	if len(items) != 2 {
		t.Fatalf("expected 2 items (original set), got %d", len(items))
	}
}

func TestStructurer_UsesCustomPattern(t *testing.T) {
	// Test that TierRegex is tried first when a custom pattern is provided
	doc := StructuredDoc{
		Sections: []DocSection{
			{Level: 1, Title: "Section 1", Text: "Text 1"},
		},
		RawText: "REQ-001: First requirement\nREQ-002: Second requirement\nREQ-003: Third requirement",
	}

	structurer := NewStructurer(nil, nil)
	opts := StructureOptions{
		SectionPattern: `^(REQ-\d+):\s+(.+)$`,
	}

	items, err := structurer.Structure(context.Background(), doc, opts)
	if err != nil {
		t.Fatalf("Structure() failed: %v", err)
	}

	// Should use TierRegex (not TierHeading) and extract 3 items
	if len(items) != 3 {
		t.Fatalf("expected 3 items from regex pattern, got %d", len(items))
	}

	if items[0].ID != "REQ-001" {
		t.Errorf("expected first item ID to be 'REQ-001', got %q", items[0].ID)
	}
}

func TestStructurer_DecompositionWithMinWords(t *testing.T) {
	// Test that decomposition respects MinDecomposeWords threshold
	doc := StructuredDoc{
		Sections: []DocSection{
			{
				Level: 1,
				Title: "Short",
				Text:  "(a) short clause", // Very short, should not be decomposed
			},
		},
		RawText: "Short\n\n(a) short clause",
	}

	structurer := NewStructurer(nil, nil)
	opts := StructureOptions{
		Decompose:         true,
		MinDecomposeWords: 40, // High threshold
	}

	items, err := structurer.Structure(context.Background(), doc, opts)
	if err != nil {
		t.Fatalf("Structure() failed: %v", err)
	}

	// Should have 1 item (not decomposed due to word count threshold)
	if len(items) != 1 {
		t.Fatalf("expected 1 item (no decomposition), got %d", len(items))
	}

	if items[0].ID != "sec-1" {
		t.Errorf("expected item ID to be 'sec-1', got %q", items[0].ID)
	}
}

func TestStructurer_CombinesFilteringAndDecomposition(t *testing.T) {
	// Test that filtering and decomposition work together
	doc := StructuredDoc{
		Sections: []DocSection{
			{
				Level: 1,
				Title: "Requirements",
				Text:  "The system shall:\n(a) implement authentication mechanisms\n(b) implement authorization controls",
			},
			{
				Level: 1,
				Title: "Background",
				Text:  "This is background information that does not contain requirements keywords.",
			},
		},
		RawText: "Requirements\n\nThe system shall:\n(a) implement authentication mechanisms\n(b) implement authorization controls\n\nBackground\n\nThis is background information that does not contain requirements keywords.",
	}

	structurer := NewStructurer(nil, nil)
	opts := StructureOptions{
		FilterByKeywords:  true,
		Keywords:          []string{"shall"},
		Decompose:         true,
		MinDecomposeWords: 5,
	}

	items, err := structurer.Structure(context.Background(), doc, opts)
	if err != nil {
		t.Fatalf("Structure() failed: %v", err)
	}

	// After filtering, should only have Requirements section
	// After decomposition, should have decomposed sub-items
	if len(items) < 2 {
		t.Fatalf("expected at least 2 items after filtering and decomposition, got %d", len(items))
	}

	// Check that all items are from the Requirements section or its decomposition
	for _, item := range items {
		if item.ID != "sec-1" && item.ID != "sec-1.a" && item.ID != "sec-1.b" {
			t.Errorf("unexpected item ID after filtering and decomposition: %q", item.ID)
		}
	}
}
