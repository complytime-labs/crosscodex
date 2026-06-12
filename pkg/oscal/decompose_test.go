package oscal

import (
	"testing"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"
)

// TestDecomposeStatements_WithStatementItems tests decomposition of a control
// with statement items into parent + children.
func TestDecomposeStatements_WithStatementItems(t *testing.T) {
	// Control with statement containing 2 items
	ctrl := oscalTypes.Control{
		ID:    "ac-2",
		Title: "Account Management",
		Parts: &[]oscalTypes.Part{
			{
				ID:    "ac-2_smt",
				Name:  "statement",
				Prose: "The organization shall:",
				Parts: &[]oscalTypes.Part{
					{
						ID:    "ac-2_smt.a",
						Name:  "item",
						Prose: "Identify and select account types.",
					},
					{
						ID:    "ac-2_smt.b",
						Name:  "item",
						Prose: "Assign account managers.",
					},
				},
			},
		},
	}

	params := make(map[string]string)
	result := DecomposeStatements(ctrl, "access-control", params)

	// Should produce 3 items: 1 parent (section) + 2 children (requirements)
	if len(result) != 3 {
		t.Fatalf("Expected 3 items, got %d", len(result))
	}

	// Check parent
	parent := result[0]
	if parent.ID != "ac-2" {
		t.Errorf("Expected parent ID 'ac-2', got '%s'", parent.ID)
	}
	if parent.Class != ClassSection {
		t.Errorf("Expected parent class '%s', got '%s'", ClassSection, parent.Class)
	}
	if parent.ParentID != "" {
		t.Errorf("Expected parent ParentID to be empty, got '%s'", parent.ParentID)
	}
	if parent.Text != "The organization shall:" {
		t.Errorf("Expected parent text 'The organization shall:', got '%s'", parent.Text)
	}

	// Check first child
	child1 := result[1]
	if child1.ID != "ac-2.a" {
		t.Errorf("Expected child1 ID 'ac-2.a', got '%s'", child1.ID)
	}
	if child1.Class != ClassRequirement {
		t.Errorf("Expected child1 class '%s', got '%s'", ClassRequirement, child1.Class)
	}
	if child1.ParentID != "ac-2" {
		t.Errorf("Expected child1 ParentID 'ac-2', got '%s'", child1.ParentID)
	}
	if child1.Props["parent-id"] != "ac-2" {
		t.Errorf("Expected child1 Props['parent-id'] 'ac-2', got '%s'", child1.Props["parent-id"])
	}

	// Check second child
	child2 := result[2]
	if child2.ID != "ac-2.b" {
		t.Errorf("Expected child2 ID 'ac-2.b', got '%s'", child2.ID)
	}
	if child2.Class != ClassRequirement {
		t.Errorf("Expected child2 class '%s', got '%s'", ClassRequirement, child2.Class)
	}
}

// TestDecomposeStatements_NestedSubItems tests handling of nested sub-items.
func TestDecomposeStatements_NestedSubItems(t *testing.T) {
	// Control with nested items (item with sub-items)
	ctrl := oscalTypes.Control{
		ID:    "ac-3",
		Title: "Access Enforcement",
		Parts: &[]oscalTypes.Part{
			{
				ID:    "ac-3_smt",
				Name:  "statement",
				Prose: "The system enforces approved authorizations for:",
				Parts: &[]oscalTypes.Part{
					{
						ID:    "ac-3_smt.a",
						Name:  "item",
						Prose: "Logical access",
						Parts: &[]oscalTypes.Part{
							{
								ID:    "ac-3_smt.a.1",
								Name:  "item",
								Prose: "Read access to system files",
							},
							{
								ID:    "ac-3_smt.a.2",
								Name:  "item",
								Prose: "Write access to system files",
							},
						},
					},
				},
			},
		},
	}

	params := make(map[string]string)
	result := DecomposeStatements(ctrl, "access-control", params)

	// Should produce 4 items:
	// 1. Parent (ac-3, section)
	// 2. Item a (ac-3.a, section because it has sub-items)
	// 3. Sub-item a.1 (ac-3.a.1, requirement)
	// 4. Sub-item a.2 (ac-3.a.2, requirement)
	if len(result) != 4 {
		t.Fatalf("Expected 4 items, got %d", len(result))
	}

	// Check parent
	if result[0].ID != "ac-3" || result[0].Class != ClassSection {
		t.Errorf("Parent should be ac-3 with ClassSection")
	}

	// Check item a (should be section because it has sub-items)
	itemA := result[1]
	if itemA.ID != "ac-3.a" {
		t.Errorf("Expected item a ID 'ac-3.a', got '%s'", itemA.ID)
	}
	if itemA.Class != ClassSection {
		t.Errorf("Expected item a class '%s' (has sub-items), got '%s'", ClassSection, itemA.Class)
	}
	if itemA.ParentID != "ac-3" {
		t.Errorf("Expected item a ParentID 'ac-3', got '%s'", itemA.ParentID)
	}

	// Check sub-item a.1
	subItem1 := result[2]
	if subItem1.ID != "ac-3.a.1" {
		t.Errorf("Expected sub-item 1 ID 'ac-3.a.1', got '%s'", subItem1.ID)
	}
	if subItem1.Class != ClassRequirement {
		t.Errorf("Expected sub-item 1 class '%s', got '%s'", ClassRequirement, subItem1.Class)
	}
	if subItem1.ParentID != "ac-3.a" {
		t.Errorf("Expected sub-item 1 ParentID 'ac-3.a', got '%s'", subItem1.ParentID)
	}

	// Check sub-item a.2
	subItem2 := result[3]
	if subItem2.ID != "ac-3.a.2" {
		t.Errorf("Expected sub-item 2 ID 'ac-3.a.2', got '%s'", subItem2.ID)
	}
	if subItem2.Class != ClassRequirement {
		t.Errorf("Expected sub-item 2 class '%s', got '%s'", ClassRequirement, subItem2.Class)
	}
}

// TestDecomposeStatements_NoStatementItems tests a control with no statement items.
func TestDecomposeStatements_NoStatementItems(t *testing.T) {
	// Control without statement
	ctrl := oscalTypes.Control{
		ID:    "ac-1",
		Title: "Policy and Procedures",
		Parts: &[]oscalTypes.Part{
			{
				ID:    "ac-1_desc",
				Name:  "description",
				Prose: "The organization develops and implements access control policies.",
			},
		},
	}

	params := make(map[string]string)
	result := DecomposeStatements(ctrl, "access-control", params)

	// Should produce single ClassRequirement item
	if len(result) != 1 {
		t.Fatalf("Expected 1 item, got %d", len(result))
	}

	item := result[0]
	if item.ID != "ac-1" {
		t.Errorf("Expected ID 'ac-1', got '%s'", item.ID)
	}
	if item.Class != ClassRequirement {
		t.Errorf("Expected class '%s', got '%s'", ClassRequirement, item.Class)
	}
	if item.ParentID != "" {
		t.Errorf("Expected empty ParentID, got '%s'", item.ParentID)
	}
}

// TestDecomposeStatements_IDDerivation tests ID derivation with _smt suffix stripping.
func TestDecomposeStatements_IDDerivation(t *testing.T) {
	ctrl := oscalTypes.Control{
		ID:    "ac-2",
		Title: "Account Management",
		Parts: &[]oscalTypes.Part{
			{
				ID:    "ac-2_smt",
				Name:  "statement",
				Prose: "The organization shall:",
				Parts: &[]oscalTypes.Part{
					{
						ID:    "ac-2_smt.a",
						Name:  "item",
						Prose: "First item",
					},
					{
						ID:    "ac-2_stmt.b", // Different suffix variant
						Name:  "item",
						Prose: "Second item",
					},
				},
			},
		},
	}

	params := make(map[string]string)
	result := DecomposeStatements(ctrl, "access-control", params)

	// Check first child ID (should strip _smt)
	if result[1].ID != "ac-2.a" {
		t.Errorf("Expected 'ac-2.a' (stripped _smt), got '%s'", result[1].ID)
	}

	// Check second child ID (should strip _stmt)
	if result[2].ID != "ac-2.b" {
		t.Errorf("Expected 'ac-2.b' (stripped _stmt), got '%s'", result[2].ID)
	}
}

// TestDecomposeStatements_FallbackID tests fallback ID when part ID has no _smt suffix.
func TestDecomposeStatements_FallbackID(t *testing.T) {
	ctrl := oscalTypes.Control{
		ID:    "ac-4",
		Title: "Information Flow Enforcement",
		Parts: &[]oscalTypes.Part{
			{
				ID:    "ac-4_smt",
				Name:  "statement",
				Prose: "The system enforces:",
				Parts: &[]oscalTypes.Part{
					{
						ID:    "", // Empty ID → should use fallback
						Name:  "item",
						Prose: "First item",
					},
					{
						ID:    "custom-id", // Non-standard ID → should use fallback
						Name:  "item",
						Prose: "Second item",
					},
				},
			},
		},
	}

	params := make(map[string]string)
	result := DecomposeStatements(ctrl, "access-control", params)

	// Check fallback ID for empty part ID
	if result[1].ID != "ac-4.1" {
		t.Errorf("Expected fallback ID 'ac-4.1', got '%s'", result[1].ID)
	}

	// Check fallback ID for non-standard part ID
	if result[2].ID != "ac-4.2" {
		t.Errorf("Expected fallback ID 'ac-4.2', got '%s'", result[2].ID)
	}
}

// TestDecomposeText_ParenthesizedClauses tests decomposition of parenthesized sub-clauses.
func TestDecomposeText_ParenthesizedClauses(t *testing.T) {
	text := "(a) First requirement here.\n(b) Second requirement here.\n(c) Third requirement here."
	result := DecomposeText("test-1", text, 5)

	// Should produce 3 requirements (tier 1 has no min word requirement)
	if len(result) != 3 {
		t.Fatalf("Expected 3 items, got %d", len(result))
	}

	// Check IDs
	expectedIDs := []string{"test-1.a", "test-1.b", "test-1.c"}
	for i, item := range result {
		if item.ID != expectedIDs[i] {
			t.Errorf("Expected ID '%s', got '%s'", expectedIDs[i], item.ID)
		}
		if item.Class != ClassRequirement {
			t.Errorf("Expected class '%s', got '%s'", ClassRequirement, item.Class)
		}
		if item.Props["label"] != string(expectedIDs[i][len(expectedIDs[i])-1]) {
			t.Errorf("Expected label '%c', got '%s'", expectedIDs[i][len(expectedIDs[i])-1], item.Props["label"])
		}
	}
}

// TestDecomposeText_NumberedParagraphs tests decomposition of numbered paragraphs.
func TestDecomposeText_NumberedParagraphs(t *testing.T) {
	text := "1. This is the first numbered paragraph with enough words to meet minimum.\n" +
		"2. This is the second numbered paragraph with enough words to meet minimum.\n" +
		"3. This is the third numbered paragraph with enough words to meet minimum."

	result := DecomposeText("test-2", text, 8)

	// Should produce 3 requirements (tier 2 requires 8 words per match)
	if len(result) != 3 {
		t.Fatalf("Expected 3 items, got %d", len(result))
	}

	// Check IDs
	expectedIDs := []string{"test-2.1", "test-2.2", "test-2.3"}
	for i, item := range result {
		if item.ID != expectedIDs[i] {
			t.Errorf("Expected ID '%s', got '%s'", expectedIDs[i], item.ID)
		}
	}
}

// TestDecomposeText_MinWords tests that text with fewer words than minimum is not decomposed.
func TestDecomposeText_MinWords(t *testing.T) {
	text := "Short text"
	result := DecomposeText("test-3", text, 10)

	// Should produce single item (text has only 2 words, minimum is 10)
	if len(result) != 1 {
		t.Fatalf("Expected 1 item (text too short), got %d", len(result))
	}

	if result[0].ID != "test-3" {
		t.Errorf("Expected ID 'test-3', got '%s'", result[0].ID)
	}
	if result[0].Class != ClassRequirement {
		t.Errorf("Expected class '%s', got '%s'", ClassRequirement, result[0].Class)
	}
}

// TestDecomposeText_RecursiveDecomposition tests recursive decomposition with nested levels.
func TestDecomposeText_RecursiveDecomposition(t *testing.T) {
	text := "(a) First item has these sub-parts:\n" +
		"1. Sub-requirement one with enough words to trigger decomposition here.\n" +
		"2. Sub-requirement two with enough words to trigger decomposition here.\n" +
		"(b) Second item is simple enough and has enough words."

	result := DecomposeText("test-4", text, 5)

	// Should produce:
	// 1. test-4.a (section, has sub-items)
	// 2. test-4.a.1 (requirement)
	// 3. test-4.a.2 (requirement)
	// 4. test-4.b (requirement, no sub-items)

	if len(result) < 4 {
		t.Fatalf("Expected at least 4 items (recursive decomposition), got %d", len(result))
	}

	// Check that first item is a section (has children)
	if result[0].Class != ClassSection {
		t.Errorf("Expected first item to be ClassSection (has sub-items), got '%s'", result[0].Class)
	}

	// Check parent-child linkage
	if result[1].ParentID != result[0].ID {
		t.Errorf("Expected child ParentID '%s', got '%s'", result[0].ID, result[1].ParentID)
	}
}

// TestWordCount tests the wordCount helper function.
func TestWordCount(t *testing.T) {
	tests := []struct {
		text     string
		expected int
	}{
		{"", 0},
		{"one", 1},
		{"one two three", 3},
		{"  multiple   spaces   between  ", 3}, // strings.Fields trims and splits properly
		{"newline\nwords\nhere", 3},
		{"tabs\tand\tspaces  mixed", 4},
	}

	for _, tt := range tests {
		result := wordCount(tt.text)
		if result != tt.expected {
			t.Errorf("wordCount(%q) = %d, expected %d", tt.text, result, tt.expected)
		}
	}
}

// TestDeriveChildID tests the deriveChildID helper function.
func TestDeriveChildID(t *testing.T) {
	tests := []struct {
		parentID string
		partID   string
		index    int
		expected string
	}{
		{"ac-2", "ac-2_smt.a", 0, "ac-2.a"},
		{"ac-2", "ac-2_stmt.a", 0, "ac-2.a"},
		{"ac-2", "ac-2.a", 0, "ac-2.a"},
		{"ac-2", "", 0, "ac-2.1"},
		{"ac-2", "custom-id", 1, "ac-2.2"},
		{"ac-3.a", "ac-3.a_smt.1", 0, "ac-3.a.1"},
	}

	for _, tt := range tests {
		part := oscalTypes.Part{ID: tt.partID}
		result := deriveChildID(tt.parentID, part, tt.index)
		if result != tt.expected {
			t.Errorf("deriveChildID(%q, %q, %d) = %q, expected %q",
				tt.parentID, tt.partID, tt.index, result, tt.expected)
		}
	}
}
