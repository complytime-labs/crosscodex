package oscal

import (
	"strings"
	"testing"
)

func TestTierRegex(t *testing.T) {
	t.Run("returns false when no pattern provided", func(t *testing.T) {
		doc := StructuredDoc{RawText: "some text"}
		opts := StructureOptions{SectionPattern: ""}

		items, ok := TierRegex(doc, opts)
		if ok {
			t.Error("expected false when SectionPattern is empty")
		}
		if items != nil {
			t.Error("expected nil items when SectionPattern is empty")
		}
	})

	t.Run("returns false on invalid regex", func(t *testing.T) {
		doc := StructuredDoc{RawText: "some text"}
		opts := StructureOptions{SectionPattern: "[invalid(regex"}

		items, ok := TierRegex(doc, opts)
		if ok {
			t.Error("expected false when regex is invalid")
		}
		if items != nil {
			t.Error("expected nil items when regex is invalid")
		}
	})

	t.Run("returns false when no matches", func(t *testing.T) {
		doc := StructuredDoc{RawText: "no matches here"}
		opts := StructureOptions{SectionPattern: `^SECTION:\s*(.+)`}

		items, ok := TierRegex(doc, opts)
		if ok {
			t.Error("expected false when no matches found")
		}
		if items != nil {
			t.Error("expected nil items when no matches found")
		}
	})

	t.Run("extracts with two capture groups (ID and text)", func(t *testing.T) {
		doc := StructuredDoc{
			RawText: `AC-1: Access Control Policy
AC-2: Account Management
AC-3: Access Enforcement`,
		}
		opts := StructureOptions{SectionPattern: `^([A-Z]+-\d+):\s*(.+)`}

		items, ok := TierRegex(doc, opts)
		if !ok {
			t.Fatal("expected true when matches found")
		}
		if len(items) != 3 {
			t.Fatalf("expected 3 items, got %d", len(items))
		}

		expected := []struct {
			id   string
			text string
		}{
			{"AC-1", "Access Control Policy"},
			{"AC-2", "Account Management"},
			{"AC-3", "Access Enforcement"},
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

	t.Run("extracts with one capture group (auto-generates ID)", func(t *testing.T) {
		doc := StructuredDoc{
			RawText: `REQUIREMENT: Must authenticate users
REQUIREMENT: Must encrypt data
REQUIREMENT: Must log access`,
		}
		opts := StructureOptions{SectionPattern: `REQUIREMENT:\s*(.+)`}

		items, ok := TierRegex(doc, opts)
		if !ok {
			t.Fatal("expected true when matches found")
		}
		if len(items) != 3 {
			t.Fatalf("expected 3 items, got %d", len(items))
		}

		expected := []struct {
			id   string
			text string
		}{
			{"sec-1", "Must authenticate users"},
			{"sec-2", "Must encrypt data"},
			{"sec-3", "Must log access"},
		}

		for i, exp := range expected {
			if items[i].ID != exp.id {
				t.Errorf("item %d: expected ID %q, got %q", i, exp.id, items[i].ID)
			}
			if items[i].Text != exp.text {
				t.Errorf("item %d: expected text %q, got %q", i, exp.text, items[i].Text)
			}
		}
	})

	t.Run("extracts with no capture groups (uses full match)", func(t *testing.T) {
		doc := StructuredDoc{
			RawText: `1.1 First requirement
2.2 Second requirement`,
		}
		opts := StructureOptions{SectionPattern: `\d+\.\d+\s+\w+\s+\w+`}

		items, ok := TierRegex(doc, opts)
		if !ok {
			t.Fatal("expected true when matches found")
		}
		if len(items) != 2 {
			t.Fatalf("expected 2 items, got %d", len(items))
		}

		if items[0].ID != "sec-1" {
			t.Errorf("expected ID sec-1, got %q", items[0].ID)
		}
		if items[0].Text != "1.1 First requirement" {
			t.Errorf("expected text %q, got %q", "1.1 First requirement", items[0].Text)
		}
	})
}

func TestTierRegex_RejectsOversizedPattern(t *testing.T) {
	doc := StructuredDoc{RawText: "1.1 First\n1.2 Second\n"}
	longPattern := strings.Repeat("a", 513)
	items, ok := TierRegex(doc, StructureOptions{SectionPattern: longPattern})
	if ok {
		t.Error("expected rejection of oversized pattern")
	}
	if items != nil {
		t.Errorf("expected nil items, got %d", len(items))
	}
}
