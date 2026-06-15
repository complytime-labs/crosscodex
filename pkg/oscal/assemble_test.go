package oscal

import (
	"context"
	"encoding/json"
	"testing"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"
)

func TestAssembler_Assemble_ValidJSON(t *testing.T) {
	assembler := NewAssembler("")
	items := []ControlItem{
		{
			ID:      "ac-1",
			Title:   "Access Control Policy",
			Text:    "Develop and document access control policy.",
			Class:   ClassRequirement,
			GroupID: "ac",
		},
	}

	meta := CatalogMeta{
		Title:   "Test Catalog",
		Version: "1.0.0",
	}

	data, err := assembler.Assemble(context.Background(), items, meta)
	if err != nil {
		t.Fatalf("Assemble failed: %v", err)
	}

	// Verify JSON is valid
	var wrapper oscalTypes.OscalCompleteSchema
	if err := json.Unmarshal(data, &wrapper); err != nil {
		t.Fatalf("Output is not valid JSON: %v", err)
	}

	// Verify catalog key exists
	if wrapper.Catalog == nil {
		t.Fatal("Expected catalog key in output")
	}

	// Verify metadata
	if wrapper.Catalog.Metadata.Title != "Test Catalog" {
		t.Errorf("Expected title 'Test Catalog', got '%s'", wrapper.Catalog.Metadata.Title)
	}
	if wrapper.Catalog.Metadata.Version != "1.0.0" {
		t.Errorf("Expected version '1.0.0', got '%s'", wrapper.Catalog.Metadata.Version)
	}
	if wrapper.Catalog.Metadata.OscalVersion != "1.1.3" {
		t.Errorf("Expected OSCAL version '1.1.3', got '%s'", wrapper.Catalog.Metadata.OscalVersion)
	}

	// Verify UUID is set
	if wrapper.Catalog.UUID == "" {
		t.Error("Expected UUID to be set")
	}

	// Verify groups exist
	if wrapper.Catalog.Groups == nil || len(*wrapper.Catalog.Groups) == 0 {
		t.Fatal("Expected at least one group")
	}

	// Verify control exists
	group := (*wrapper.Catalog.Groups)[0]
	if group.Controls == nil || len(*group.Controls) == 0 {
		t.Fatal("Expected at least one control in group")
	}

	ctrl := (*group.Controls)[0]
	if ctrl.ID != "ac-1" {
		t.Errorf("Expected control ID 'ac-1', got '%s'", ctrl.ID)
	}
	if ctrl.Title != "Access Control Policy" {
		t.Errorf("Expected control title 'Access Control Policy', got '%s'", ctrl.Title)
	}
}

func TestAssembler_GroupsByGroupID(t *testing.T) {
	assembler := NewAssembler("")
	items := []ControlItem{
		{
			ID:      "ac-1",
			Title:   "Access Control Policy",
			Text:    "Develop access control policy.",
			GroupID: "ac",
		},
		{
			ID:      "ac-2",
			Title:   "Account Management",
			Text:    "Manage accounts.",
			GroupID: "ac",
		},
		{
			ID:      "au-1",
			Title:   "Audit Policy",
			Text:    "Develop audit policy.",
			GroupID: "au",
		},
	}

	meta := CatalogMeta{
		Title:   "Multi-Group Catalog",
		Version: "1.0.0",
	}

	data, err := assembler.Assemble(context.Background(), items, meta)
	if err != nil {
		t.Fatalf("Assemble failed: %v", err)
	}

	var wrapper oscalTypes.OscalCompleteSchema
	if err := json.Unmarshal(data, &wrapper); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}

	if wrapper.Catalog.Groups == nil {
		t.Fatal("Expected groups to be set")
	}

	groups := *wrapper.Catalog.Groups
	if len(groups) != 2 {
		t.Fatalf("Expected 2 groups, got %d", len(groups))
	}

	// Verify each group has correct controls
	groupIDs := make(map[string]int)
	for _, group := range groups {
		if group.Controls != nil {
			groupIDs[group.ID] = len(*group.Controls)
		}
	}

	if groupIDs["ac"] != 2 {
		t.Errorf("Expected 2 controls in 'ac' group, got %d", groupIDs["ac"])
	}
	if groupIDs["au"] != 1 {
		t.Errorf("Expected 1 control in 'au' group, got %d", groupIDs["au"])
	}
}

func TestAssembler_ParentChildWiring(t *testing.T) {
	assembler := NewAssembler("")
	items := []ControlItem{
		{
			ID:      "ac-1",
			Title:   "Access Control Policy",
			Text:    "Parent control",
			GroupID: "ac",
		},
		{
			ID:       "ac-1.a",
			Title:    "Policy development",
			Text:     "Child control A",
			ParentID: "ac-1",
			GroupID:  "ac",
		},
		{
			ID:       "ac-1.b",
			Title:    "Policy dissemination",
			Text:     "Child control B",
			ParentID: "ac-1",
			GroupID:  "ac",
		},
		{
			ID:       "ac-1.b.1",
			Title:    "Dissemination methods",
			Text:     "Grandchild control",
			ParentID: "ac-1.b",
			GroupID:  "ac",
		},
	}

	meta := CatalogMeta{
		Title:   "Parent-Child Catalog",
		Version: "1.0.0",
	}

	data, err := assembler.Assemble(context.Background(), items, meta)
	if err != nil {
		t.Fatalf("Assemble failed: %v", err)
	}

	var wrapper oscalTypes.OscalCompleteSchema
	if err := json.Unmarshal(data, &wrapper); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}

	// Navigate to ac-1 control
	if wrapper.Catalog.Groups == nil || len(*wrapper.Catalog.Groups) == 0 {
		t.Fatal("Expected at least one group")
	}

	group := (*wrapper.Catalog.Groups)[0]
	if group.Controls == nil || len(*group.Controls) == 0 {
		t.Fatal("Expected at least one control")
	}

	ctrl := (*group.Controls)[0]
	if ctrl.ID != "ac-1" {
		t.Fatalf("Expected control ID 'ac-1', got '%s'", ctrl.ID)
	}

	// Verify children exist as parts
	if ctrl.Parts == nil {
		t.Fatal("Expected parts to be set for parent control")
	}

	parts := *ctrl.Parts
	// Should have statement part (from ItemToControl) + 2 child parts
	var childParts []oscalTypes.Part
	for _, part := range parts {
		if part.Name == "item" {
			childParts = append(childParts, part)
		}
	}

	if len(childParts) != 2 {
		t.Fatalf("Expected 2 child parts, got %d", len(childParts))
	}

	// Verify ac-1.b has nested child
	var ac1b *oscalTypes.Part
	for _, part := range childParts {
		if part.ID == "ac-1.b" {
			ac1b = &part
			break
		}
	}

	if ac1b == nil {
		t.Fatal("Expected to find ac-1.b part")
	}

	if ac1b.Parts == nil || len(*ac1b.Parts) != 1 {
		t.Fatal("Expected ac-1.b to have 1 nested part (ac-1.b.1)")
	}

	grandchild := (*ac1b.Parts)[0]
	if grandchild.ID != "ac-1.b.1" {
		t.Errorf("Expected grandchild ID 'ac-1.b.1', got '%s'", grandchild.ID)
	}
}

func TestAssembler_EmptyGroupID(t *testing.T) {
	assembler := NewAssembler("")
	items := []ControlItem{
		{
			ID:      "misc-1",
			Title:   "Miscellaneous Control",
			Text:    "No group specified",
			GroupID: "",
		},
	}

	meta := CatalogMeta{
		Title:   "Default Group Catalog",
		Version: "1.0.0",
	}

	data, err := assembler.Assemble(context.Background(), items, meta)
	if err != nil {
		t.Fatalf("Assemble failed: %v", err)
	}

	var wrapper oscalTypes.OscalCompleteSchema
	if err := json.Unmarshal(data, &wrapper); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}

	// Verify default group exists
	if wrapper.Catalog.Groups == nil || len(*wrapper.Catalog.Groups) == 0 {
		t.Fatal("Expected at least one group")
	}

	group := (*wrapper.Catalog.Groups)[0]
	if group.ID != "default" {
		t.Errorf("Expected group ID 'default', got '%s'", group.ID)
	}
}

func TestAssembler_WellFormedJSON(t *testing.T) {
	assembler := NewAssembler("")
	items := []ControlItem{
		{
			ID:      "test-1",
			Title:   "Test Control",
			Text:    "Test prose with special chars: <>&\"'",
			GroupID: "test",
			Props: map[string]string{
				"key1": "value1",
				"key2": "value2",
			},
		},
	}

	meta := CatalogMeta{
		Title:   "Test Catalog",
		Version: "1.0.0",
	}

	data, err := assembler.Assemble(context.Background(), items, meta)
	if err != nil {
		t.Fatalf("Assemble failed: %v", err)
	}

	// Verify JSON can be unmarshalled
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("Failed to unmarshal as generic JSON: %v", err)
	}

	// Verify catalog key exists
	if _, ok := result["catalog"]; !ok {
		t.Error("Expected 'catalog' key in JSON output")
	}

	// Verify it's also valid OSCAL structure
	var wrapper oscalTypes.OscalCompleteSchema
	if err := json.Unmarshal(data, &wrapper); err != nil {
		t.Fatalf("Failed to unmarshal as OSCAL: %v", err)
	}

	if wrapper.Catalog == nil {
		t.Error("Expected catalog to be set")
	}
}
