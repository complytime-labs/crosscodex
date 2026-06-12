package oscal

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

func TestParser_Parse_MinimalCatalog(t *testing.T) {
	data, err := os.ReadFile("testdata/minimal_catalog.json")
	if err != nil {
		t.Fatalf("failed to read test fixture: %v", err)
	}

	parser := NewParser("")
	items, err := parser.Parse(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	// Should produce more than 3 items (AC-1, AC-2 parent + children, AC-2.1, AC-3)
	if len(items) < 3 {
		t.Errorf("Parse() produced %d items, expected at least 3", len(items))
	}

	t.Logf("Parsed %d control items", len(items))
}

func TestParser_Parse_DecomposesAC2(t *testing.T) {
	data, err := os.ReadFile("testdata/minimal_catalog.json")
	if err != nil {
		t.Fatalf("failed to read test fixture: %v", err)
	}

	parser := NewParser("")
	items, err := parser.Parse(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	// Find AC-2 parent
	var ac2Parent *ControlItem
	for i := range items {
		if items[i].ID == "ac-2" {
			ac2Parent = &items[i]
			break
		}
	}

	if ac2Parent == nil {
		t.Fatal("AC-2 parent not found in parsed items")
	}

	// Verify AC-2 parent is ClassSection
	if ac2Parent.Class != ClassSection {
		t.Errorf("AC-2 parent Class = %s, expected %s", ac2Parent.Class, ClassSection)
	}

	// Verify AC-2 parent has correct title
	if ac2Parent.Title != "Account Management" {
		t.Errorf("AC-2 parent Title = %q, expected %q", ac2Parent.Title, "Account Management")
	}

	// Find AC-2.a child
	var ac2a *ControlItem
	for i := range items {
		if items[i].ID == "ac-2.a" {
			ac2a = &items[i]
			break
		}
	}

	if ac2a == nil {
		t.Fatal("AC-2.a child not found in parsed items")
	}

	// Verify AC-2.a is ClassRequirement
	if ac2a.Class != ClassRequirement {
		t.Errorf("AC-2.a Class = %s, expected %s", ac2a.Class, ClassRequirement)
	}

	// Verify AC-2.a has correct parent
	if ac2a.ParentID != "ac-2" {
		t.Errorf("AC-2.a ParentID = %q, expected %q", ac2a.ParentID, "ac-2")
	}

	// Verify AC-2.c exists (has sub-items, should be ClassSection)
	var ac2c *ControlItem
	for i := range items {
		if items[i].ID == "ac-2.c" {
			ac2c = &items[i]
			break
		}
	}

	if ac2c == nil {
		t.Fatal("AC-2.c not found in parsed items")
	}

	if ac2c.Class != ClassSection {
		t.Errorf("AC-2.c Class = %s, expected %s (should be a section because it has sub-items)", ac2c.Class, ClassSection)
	}

	// Verify AC-2.c.1 exists
	var ac2c1 *ControlItem
	for i := range items {
		if items[i].ID == "ac-2.c.1" {
			ac2c1 = &items[i]
			break
		}
	}

	if ac2c1 == nil {
		t.Fatal("AC-2.c.1 not found in parsed items")
	}

	if ac2c1.Class != ClassRequirement {
		t.Errorf("AC-2.c.1 Class = %s, expected %s", ac2c1.Class, ClassRequirement)
	}

	if ac2c1.ParentID != "ac-2.c" {
		t.Errorf("AC-2.c.1 ParentID = %q, expected %q", ac2c1.ParentID, "ac-2.c")
	}

	t.Logf("AC-2 decomposition verified: parent=%s, children=%d total items", ac2Parent.ID, len(items))
}

func TestParser_Parse_SubstitutesParameters(t *testing.T) {
	data, err := os.ReadFile("testdata/minimal_catalog.json")
	if err != nil {
		t.Fatalf("failed to read test fixture: %v", err)
	}

	parser := NewParser("")
	items, err := parser.Parse(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	// Find AC-1
	var ac1 *ControlItem
	for i := range items {
		if items[i].ID == "ac-1" {
			ac1 = &items[i]
			break
		}
	}

	if ac1 == nil {
		t.Fatal("AC-1 not found in parsed items")
	}

	// Verify parameter substitution occurred
	// ac-1_prm_1 should be substituted with label "organization-defined personnel or roles" (from catalog-level params)
	// (label takes precedence over values array per CollectParams logic)
	if !strings.Contains(ac1.Text, "organization-defined personnel or roles") {
		t.Errorf("AC-1 text missing expected parameter substitution 'organization-defined personnel or roles': %q", ac1.Text)
	}

	// ac-1_prm_2 should be substituted with "organization-defined frequency" (from control-level params)
	if !strings.Contains(ac1.Text, "organization-defined frequency") {
		t.Errorf("AC-1 text missing expected parameter substitution 'organization-defined frequency': %q", ac1.Text)
	}

	// Should NOT contain unsubstituted template markers
	if strings.Contains(ac1.Text, "{{ insert:") {
		t.Errorf("AC-1 text contains unsubstituted template marker: %q", ac1.Text)
	}

	t.Logf("AC-1 text after parameter substitution: %q", ac1.Text)
}

func TestParser_Parse_IncludesSubControls(t *testing.T) {
	data, err := os.ReadFile("testdata/minimal_catalog.json")
	if err != nil {
		t.Fatalf("failed to read test fixture: %v", err)
	}

	parser := NewParser("")
	items, err := parser.Parse(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	// Find AC-2.1 (sub-control/enhancement)
	var ac21 *ControlItem
	for i := range items {
		if items[i].ID == "ac-2.1" {
			ac21 = &items[i]
			break
		}
	}

	if ac21 == nil {
		t.Fatal("AC-2.1 (sub-control) not found in parsed items")
	}

	// Verify it has the correct title
	if ac21.Title != "Automated System Account Management" {
		t.Errorf("AC-2.1 Title = %q, expected %q", ac21.Title, "Automated System Account Management")
	}

	// Verify it has text
	if ac21.Text == "" {
		t.Error("AC-2.1 Text is empty")
	}

	t.Logf("AC-2.1 sub-control verified: %s", ac21.ID)
}

func TestParser_Parse_EmptyCatalog(t *testing.T) {
	emptyDoc := `{
		"catalog": {
			"uuid": "00000000-0000-0000-0000-000000000000",
			"metadata": {
				"title": "Empty Catalog",
				"version": "1.0",
				"oscal-version": "1.1.3",
				"last-modified": "2026-06-11T00:00:00Z"
			}
		}
	}`

	parser := NewParser("")
	_, err := parser.Parse(context.Background(), strings.NewReader(emptyDoc))

	if err != ErrNoControls {
		t.Errorf("Parse() with empty catalog returned error %v, expected %v", err, ErrNoControls)
	}
}

func TestParser_Parse_InvalidJSON(t *testing.T) {
	invalidJSON := `{ "catalog": { "uuid": "invalid`

	parser := NewParser("")
	_, err := parser.Parse(context.Background(), strings.NewReader(invalidJSON))

	if err == nil {
		t.Fatal("Parse() with invalid JSON should return an error")
	}

	if !strings.Contains(err.Error(), "failed to parse OSCAL catalog") {
		t.Errorf("Parse() error = %v, expected parse failure message", err)
	}
}

func TestParser_Parse_NotACatalog(t *testing.T) {
	profileDoc := `{
		"profile": {
			"uuid": "00000000-0000-0000-0000-000000000000",
			"metadata": {
				"title": "Test Profile",
				"version": "1.0",
				"oscal-version": "1.1.3",
				"last-modified": "2026-06-11T00:00:00Z"
			}
		}
	}`

	parser := NewParser("")
	_, err := parser.Parse(context.Background(), strings.NewReader(profileDoc))

	if err != ErrInvalidFormat {
		t.Errorf("Parse() with non-catalog document returned error %v, expected %v", err, ErrInvalidFormat)
	}
}

func TestParser_FindControl(t *testing.T) {
	data, err := os.ReadFile("testdata/minimal_catalog.json")
	if err != nil {
		t.Fatalf("failed to read test fixture: %v", err)
	}

	parser := NewParser("")
	items, err := parser.Parse(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	tests := []struct {
		name      string
		controlID string
		wantErr   bool
	}{
		{
			name:      "find existing control",
			controlID: "ac-2",
			wantErr:   false,
		},
		{
			name:      "find sub-control",
			controlID: "ac-2.1",
			wantErr:   false,
		},
		{
			name:      "find child item",
			controlID: "ac-2.a",
			wantErr:   false,
		},
		{
			name:      "control not found",
			controlID: "nonexistent",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			found, err := parser.FindControl(items, tt.controlID)
			if (err != nil) != tt.wantErr {
				t.Errorf("FindControl(%q) error = %v, wantErr %v", tt.controlID, err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if found == nil {
					t.Errorf("FindControl(%q) returned nil, expected item", tt.controlID)
					return
				}
				if found.ID != tt.controlID {
					t.Errorf("FindControl(%q) returned ID = %q, expected %q", tt.controlID, found.ID, tt.controlID)
				}
			}
		})
	}
}

func TestParser_Parse_GuidanceFiltered(t *testing.T) {
	data, err := os.ReadFile("testdata/minimal_catalog.json")
	if err != nil {
		t.Fatalf("failed to read test fixture: %v", err)
	}

	parser := NewParser("")
	items, err := parser.Parse(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	// Find AC-1
	var ac1 *ControlItem
	for i := range items {
		if items[i].ID == "ac-1" {
			ac1 = &items[i]
			break
		}
	}

	if ac1 == nil {
		t.Fatal("AC-1 not found in parsed items")
	}

	// Verify guidance text is NOT included (only statement parts should be parsed)
	if strings.Contains(ac1.Text, "This is guidance text") {
		t.Errorf("AC-1 text contains guidance text, should only include statement parts: %q", ac1.Text)
	}

	t.Logf("AC-1 correctly excludes guidance text: %q", ac1.Text)
}
