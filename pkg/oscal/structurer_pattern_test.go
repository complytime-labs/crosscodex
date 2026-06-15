package oscal

import (
	"testing"
)

func TestTierPattern(t *testing.T) {
	t.Run("returns false when no pattern matches", func(t *testing.T) {
		doc := StructuredDoc{
			RawText: `Just some random text
with no discernible pattern
at all`,
		}
		opts := StructureOptions{}

		items, ok := TierPattern(doc, opts)
		if ok {
			t.Error("expected false when no pattern matches")
		}
		if items != nil {
			t.Error("expected nil items when no pattern matches")
		}
	})

	t.Run("detects pattern 1: numeric dot notation (1.1, 2.3)", func(t *testing.T) {
		doc := StructuredDoc{
			RawText: `1.1 First requirement
1.2 Second requirement
2.1 Third requirement`,
		}
		opts := StructureOptions{}

		items, ok := TierPattern(doc, opts)
		if !ok {
			t.Fatal("expected true when pattern matches")
		}
		if len(items) != 3 {
			t.Fatalf("expected 3 items, got %d", len(items))
		}

		expectedIDs := []string{"1.1", "1.2", "2.1"}
		for i, expID := range expectedIDs {
			if items[i].ID != expID {
				t.Errorf("item %d: expected ID %q, got %q", i, expID, items[i].ID)
			}
			if items[i].Class != ClassRequirement {
				t.Errorf("item %d: expected Class %q, got %q", i, ClassRequirement, items[i].Class)
			}
		}
	})

	t.Run("detects pattern 2: letter dot number (A.1, B.2)", func(t *testing.T) {
		doc := StructuredDoc{
			RawText: `A.1 First rule
A.2 Second rule
B.1 Third rule`,
		}
		opts := StructureOptions{}

		items, ok := TierPattern(doc, opts)
		if !ok {
			t.Fatal("expected true when pattern matches")
		}
		if len(items) != 3 {
			t.Fatalf("expected 3 items, got %d", len(items))
		}

		expectedIDs := []string{"A.1", "A.2", "B.1"}
		for i, expID := range expectedIDs {
			if items[i].ID != expID {
				t.Errorf("item %d: expected ID %q, got %q", i, expID, items[i].ID)
			}
		}
	})

	t.Run("detects pattern 3: Article N", func(t *testing.T) {
		doc := StructuredDoc{
			RawText: `Article 1 - Data Protection
Article 2 - Privacy Rights
Article 3 - User Consent`,
		}
		opts := StructureOptions{}

		items, ok := TierPattern(doc, opts)
		if !ok {
			t.Fatal("expected true when pattern matches")
		}
		if len(items) != 3 {
			t.Fatalf("expected 3 items, got %d", len(items))
		}

		expectedIDs := []string{"Article 1", "Article 2", "Article 3"}
		for i, expID := range expectedIDs {
			if items[i].ID != expID {
				t.Errorf("item %d: expected ID %q, got %q", i, expID, items[i].ID)
			}
		}
	})

	t.Run("detects pattern 4: Section N", func(t *testing.T) {
		doc := StructuredDoc{
			RawText: `Section 1 Introduction
Section 2 Requirements
Section 3 Compliance`,
		}
		opts := StructureOptions{}

		items, ok := TierPattern(doc, opts)
		if !ok {
			t.Fatal("expected true when pattern matches")
		}
		if len(items) != 3 {
			t.Fatalf("expected 3 items, got %d", len(items))
		}
	})

	t.Run("detects pattern 5: Rule N", func(t *testing.T) {
		doc := StructuredDoc{
			RawText: `Rule 1 Authentication required
Rule 2 Authorization required
Rule 3 Audit logging required`,
		}
		opts := StructureOptions{}

		items, ok := TierPattern(doc, opts)
		if !ok {
			t.Fatal("expected true when pattern matches")
		}
		if len(items) != 3 {
			t.Fatalf("expected 3 items, got %d", len(items))
		}
	})

	t.Run("detects pattern 6: section symbol (§1, § 2)", func(t *testing.T) {
		doc := StructuredDoc{
			RawText: `§1 Legal requirement
§2 Regulatory compliance
§ 3 Audit trail`,
		}
		opts := StructureOptions{}

		items, ok := TierPattern(doc, opts)
		if !ok {
			t.Fatal("expected true when pattern matches")
		}
		if len(items) != 3 {
			t.Fatalf("expected 3 items, got %d", len(items))
		}
	})

	t.Run("detects pattern 7: lowercase letter in parens (a), (b)", func(t *testing.T) {
		doc := StructuredDoc{
			RawText: `(a) First clause
(b) Second clause
(c) Third clause`,
		}
		opts := StructureOptions{}

		items, ok := TierPattern(doc, opts)
		if !ok {
			t.Fatal("expected true when pattern matches")
		}
		if len(items) != 3 {
			t.Fatalf("expected 3 items, got %d", len(items))
		}

		expectedIDs := []string{"(a)", "(b)", "(c)"}
		for i, expID := range expectedIDs {
			if items[i].ID != expID {
				t.Errorf("item %d: expected ID %q, got %q", i, expID, items[i].ID)
			}
		}
	})

	t.Run("detects pattern 8: number in parens (1), (2)", func(t *testing.T) {
		doc := StructuredDoc{
			RawText: `(1) First item
(2) Second item
(3) Third item`,
		}
		opts := StructureOptions{}

		items, ok := TierPattern(doc, opts)
		if !ok {
			t.Fatal("expected true when pattern matches")
		}
		if len(items) != 3 {
			t.Fatalf("expected 3 items, got %d", len(items))
		}
	})

	t.Run("detects pattern 9: roman numerals (i., ii., iii.)", func(t *testing.T) {
		doc := StructuredDoc{
			RawText: `i. First point
ii. Second point
iii. Third point`,
		}
		opts := StructureOptions{}

		items, ok := TierPattern(doc, opts)
		if !ok {
			t.Fatal("expected true when pattern matches")
		}
		if len(items) != 3 {
			t.Fatalf("expected 3 items, got %d", len(items))
		}
	})

	t.Run("detects pattern 10: lowercase letter dot (a., b., c.)", func(t *testing.T) {
		doc := StructuredDoc{
			RawText: `a. Alpha requirement
b. Beta requirement
c. Gamma requirement`,
		}
		opts := StructureOptions{}

		items, ok := TierPattern(doc, opts)
		if !ok {
			t.Fatal("expected true when pattern matches")
		}
		if len(items) != 3 {
			t.Fatalf("expected 3 items, got %d", len(items))
		}
	})

	t.Run("detects pattern 11: uppercase letter dot (A., B., C.)", func(t *testing.T) {
		doc := StructuredDoc{
			RawText: `A. First requirement
B. Second requirement
C. Third requirement`,
		}
		opts := StructureOptions{}

		items, ok := TierPattern(doc, opts)
		if !ok {
			t.Fatal("expected true when pattern matches")
		}
		if len(items) != 3 {
			t.Fatalf("expected 3 items, got %d", len(items))
		}
	})

	t.Run("detects pattern 12: simple number dot (1., 2., 3.)", func(t *testing.T) {
		doc := StructuredDoc{
			RawText: `1. First item
2. Second item
3. Third item`,
		}
		opts := StructureOptions{}

		items, ok := TierPattern(doc, opts)
		if !ok {
			t.Fatal("expected true when pattern matches")
		}
		if len(items) != 3 {
			t.Fatalf("expected 3 items, got %d", len(items))
		}
	})

	t.Run("requires at least 3 matches", func(t *testing.T) {
		doc := StructuredDoc{
			RawText: `1. First item
2. Second item
Some random text`,
		}
		opts := StructureOptions{}

		items, ok := TierPattern(doc, opts)
		if ok {
			t.Error("expected false when fewer than 3 matches")
		}
		if items != nil {
			t.Error("expected nil items when fewer than 3 matches")
		}
	})

	t.Run("selects first matching pattern", func(t *testing.T) {
		doc := StructuredDoc{
			RawText: `1.1 Higher priority pattern
1.2 Should match this
1.3 Not the lower priority
1. This would match pattern 12 but pattern 1 has priority`,
		}
		opts := StructureOptions{}

		items, ok := TierPattern(doc, opts)
		if !ok {
			t.Fatal("expected true when pattern matches")
		}
		// Should match pattern 1 (^\d+\.\d+) which appears 3 times
		if len(items) != 3 {
			t.Fatalf("expected 3 items from pattern 1, got %d", len(items))
		}
		if items[0].ID != "1.1" {
			t.Errorf("expected ID from pattern 1, got %q", items[0].ID)
		}
	})
}
