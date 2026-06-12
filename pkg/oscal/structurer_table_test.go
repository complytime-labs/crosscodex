package oscal

import (
	"testing"
)

func TestTierTable(t *testing.T) {
	t.Run("returns false when no tables", func(t *testing.T) {
		doc := StructuredDoc{Tables: []DocTable{}}
		opts := StructureOptions{}

		items, ok := TierTable(doc, opts)
		if ok {
			t.Error("expected false when no tables present")
		}
		if items != nil {
			t.Error("expected nil items when no tables present")
		}
	})

	t.Run("extracts rows with ID from first column", func(t *testing.T) {
		doc := StructuredDoc{
			Tables: []DocTable{
				{
					Headers: []string{"ID", "Description", "Status"},
					Rows: [][]string{
						{"AC-1", "Access Control Policy", "Active"},
						{"AC-2", "Account Management", "Active"},
					},
				},
			},
		}
		opts := StructureOptions{}

		items, ok := TierTable(doc, opts)
		if !ok {
			t.Fatal("expected true when tables found")
		}
		if len(items) != 2 {
			t.Fatalf("expected 2 items, got %d", len(items))
		}

		expected := []struct {
			id   string
			text string
		}{
			{"AC-1", "AC-1 | Access Control Policy | Active"},
			{"AC-2", "AC-2 | Account Management | Active"},
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

	t.Run("auto-generates ID when first column is empty or too long", func(t *testing.T) {
		doc := StructuredDoc{
			Tables: []DocTable{
				{
					Headers: []string{"Description", "Notes"},
					Rows: [][]string{
						{"", "Empty ID row"},
						{"This is a very long first column that exceeds fifty characters limit", "Long ID row"},
					},
				},
			},
		}
		opts := StructureOptions{}

		items, ok := TierTable(doc, opts)
		if !ok {
			t.Fatal("expected true when tables found")
		}
		if len(items) != 2 {
			t.Fatalf("expected 2 items, got %d", len(items))
		}

		if items[0].ID != "tbl-1" {
			t.Errorf("item 0: expected auto-generated ID tbl-1, got %q", items[0].ID)
		}
		if items[1].ID != "tbl-2" {
			t.Errorf("item 1: expected auto-generated ID tbl-2, got %q", items[1].ID)
		}
	})

	t.Run("handles multiple tables", func(t *testing.T) {
		doc := StructuredDoc{
			Tables: []DocTable{
				{
					Headers: []string{"ID", "Desc"},
					Rows: [][]string{
						{"A-1", "First table"},
					},
				},
				{
					Headers: []string{"ID", "Desc"},
					Rows: [][]string{
						{"B-1", "Second table"},
						{"B-2", "Second table row 2"},
					},
				},
			},
		}
		opts := StructureOptions{}

		items, ok := TierTable(doc, opts)
		if !ok {
			t.Fatal("expected true when tables found")
		}
		if len(items) != 3 {
			t.Fatalf("expected 3 items, got %d", len(items))
		}

		expected := []string{"A-1", "B-1", "B-2"}
		for i, expID := range expected {
			if items[i].ID != expID {
				t.Errorf("item %d: expected ID %q, got %q", i, expID, items[i].ID)
			}
		}
	})

	t.Run("skips empty rows", func(t *testing.T) {
		doc := StructuredDoc{
			Tables: []DocTable{
				{
					Headers: []string{"ID", "Desc"},
					Rows: [][]string{
						{}, // Empty row
						{"A-1", "Valid row"},
						{}, // Another empty row
					},
				},
			},
		}
		opts := StructureOptions{}

		items, ok := TierTable(doc, opts)
		if !ok {
			t.Fatal("expected true when valid rows found")
		}
		if len(items) != 1 {
			t.Fatalf("expected 1 item (empty rows skipped), got %d", len(items))
		}
		if items[0].ID != "A-1" {
			t.Errorf("expected ID A-1, got %q", items[0].ID)
		}
	})

	t.Run("returns false when all rows are empty", func(t *testing.T) {
		doc := StructuredDoc{
			Tables: []DocTable{
				{
					Headers: []string{"ID", "Desc"},
					Rows:    [][]string{{}, {}},
				},
			},
		}
		opts := StructureOptions{}

		items, ok := TierTable(doc, opts)
		if ok {
			t.Error("expected false when all rows are empty")
		}
		if items != nil {
			t.Error("expected nil items when all rows are empty")
		}
	})
}
