package oscal

import (
	"testing"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"
)

// Test 1: ControlToItem converts a basic Control correctly (ID, Title, GroupID)
func TestControlToItem_BasicFields(t *testing.T) {
	ctrl := oscalTypes.Control{
		ID:    "ac-1",
		Title: "Policy and Procedures",
	}

	item := ControlToItem(ctrl, "ac", nil)

	if item.ID != "ac-1" {
		t.Errorf("expected ID 'ac-1', got %q", item.ID)
	}
	if item.Title != "Policy and Procedures" {
		t.Errorf("expected Title 'Policy and Procedures', got %q", item.Title)
	}
	if item.GroupID != "ac" {
		t.Errorf("expected GroupID 'ac', got %q", item.GroupID)
	}
	if item.Class != ClassRequirement {
		t.Errorf("expected Class %q, got %q", ClassRequirement, item.Class)
	}
	if item.Props == nil {
		t.Error("expected Props to be initialized, got nil")
	}
}

// Test 2: ControlToItem extracts prose from statement parts only (not guidance)
func TestControlToItem_ExtractsStatementOnly(t *testing.T) {
	parts := []oscalTypes.Part{
		{
			Name:  "guidance",
			Prose: "This is guidance text that should be ignored.",
		},
		{
			Name:  "statement",
			Prose: "This is the control statement.",
		},
		{
			Name:  "other",
			Prose: "This should also be ignored.",
		},
	}

	ctrl := oscalTypes.Control{
		ID:    "ac-2",
		Title: "Account Management",
		Parts: &parts,
	}

	item := ControlToItem(ctrl, "ac", nil)

	expected := "This is the control statement."
	if item.Text != expected {
		t.Errorf("expected Text %q, got %q", expected, item.Text)
	}
}

// Test 3: ControlToItem handles control with no parts gracefully
func TestControlToItem_NoParts(t *testing.T) {
	ctrl := oscalTypes.Control{
		ID:    "ac-3",
		Title: "Access Enforcement",
		Parts: nil,
	}

	item := ControlToItem(ctrl, "ac", nil)

	if item.Text != "" {
		t.Errorf("expected empty Text, got %q", item.Text)
	}
	if item.Class != ClassRequirement {
		t.Errorf("expected Class %q, got %q", ClassRequirement, item.Class)
	}
}

// Test 4: ControlToItem recursively extracts nested statement prose
func TestControlToItem_NestedStatementProse(t *testing.T) {
	subParts := []oscalTypes.Part{
		{Name: "item", Prose: "Sub-requirement a."},
		{Name: "item", Prose: "Sub-requirement b."},
	}
	parts := []oscalTypes.Part{
		{
			Name:  "statement",
			Prose: "Main requirement:",
			Parts: &subParts,
		},
	}

	ctrl := oscalTypes.Control{
		ID:    "ac-4",
		Title: "Information Flow Enforcement",
		Parts: &parts,
	}

	item := ControlToItem(ctrl, "ac", nil)

	expected := "Main requirement:\nSub-requirement a.\nSub-requirement b."
	if item.Text != expected {
		t.Errorf("expected Text:\n%q\ngot:\n%q", expected, item.Text)
	}
}

// Test 5: ControlToItem applies parameter substitution when params provided
func TestControlToItem_ParameterSubstitution(t *testing.T) {
	parts := []oscalTypes.Part{
		{
			Name:  "statement",
			Prose: "Review access every {{ insert: param, ac-2_prm_1 }} days.",
		},
	}

	ctrl := oscalTypes.Control{
		ID:    "ac-5",
		Title: "Separation of Duties",
		Parts: &parts,
	}

	params := map[string]string{
		"ac-2_prm_1": "90",
	}

	item := ControlToItem(ctrl, "ac", params)

	expected := "Review access every 90 days."
	if item.Text != expected {
		t.Errorf("expected Text %q, got %q", expected, item.Text)
	}
}

// Test 6: ItemToControl round-trips ID, Title correctly
func TestItemToControl_BasicRoundTrip(t *testing.T) {
	item := ControlItem{
		ID:      "ac-6",
		Title:   "Least Privilege",
		Text:    "Employ least privilege principle.",
		GroupID: "ac",
		Class:   ClassRequirement,
		Props:   make(map[string]string),
	}

	ctrl := ItemToControl(item)

	if ctrl.ID != "ac-6" {
		t.Errorf("expected ID 'ac-6', got %q", ctrl.ID)
	}
	if ctrl.Title != "Least Privilege" {
		t.Errorf("expected Title 'Least Privilege', got %q", ctrl.Title)
	}

	if ctrl.Parts == nil {
		t.Fatal("expected Parts to be non-nil")
	}
	if len(*ctrl.Parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(*ctrl.Parts))
	}

	part := (*ctrl.Parts)[0]
	if part.Name != "statement" {
		t.Errorf("expected part Name 'statement', got %q", part.Name)
	}
	if part.Prose != "Employ least privilege principle." {
		t.Errorf("expected part Prose %q, got %q", "Employ least privilege principle.", part.Prose)
	}
}

// Test 7: ItemToControl sets compliance-mapper namespace on props
func TestItemToControl_ComplianceMapperNamespace(t *testing.T) {
	item := ControlItem{
		ID:    "ac-7",
		Title: "Unsuccessful Logon Attempts",
		Props: map[string]string{
			"custom-key":   "custom-value",
			"another-prop": "another-value",
		},
	}

	ctrl := ItemToControl(item)

	if ctrl.Props == nil {
		t.Fatal("expected Props to be non-nil")
	}

	props := *ctrl.Props
	if len(props) != 2 {
		t.Fatalf("expected 2 properties, got %d", len(props))
	}

	// Verify namespace and values
	nsCount := 0
	for _, prop := range props {
		if prop.Ns != complianceMapperNS {
			t.Errorf("expected namespace %q, got %q", complianceMapperNS, prop.Ns)
		}
		nsCount++

		// Check that our props are present
		if prop.Name == "custom-key" && prop.Value != "custom-value" {
			t.Errorf("expected value 'custom-value', got %q", prop.Value)
		}
		if prop.Name == "another-prop" && prop.Value != "another-value" {
			t.Errorf("expected value 'another-value', got %q", prop.Value)
		}
	}

	if nsCount != 2 {
		t.Errorf("expected 2 properties with namespace, got %d", nsCount)
	}
}

// Test 8: CollectParams merges catalog and control params (control overrides catalog)
func TestCollectParams_MergesCatalogAndControl(t *testing.T) {
	catalogParams := []oscalTypes.Parameter{
		{ID: "param-1", Label: "Catalog Default"},
		{ID: "param-2", Values: &[]string{"catalog-value"}},
	}

	controlParams := []oscalTypes.Parameter{
		{ID: "param-1", Label: "Control Override"}, // Override param-1
		{ID: "param-3", Label: "Control Only"},
	}

	catalog := &oscalTypes.Catalog{
		UUID:   "test-catalog",
		Params: &catalogParams,
	}

	ctrl := oscalTypes.Control{
		ID:     "ac-8",
		Title:  "System Use Notification",
		Params: &controlParams,
	}

	params := CollectParams(catalog, ctrl)

	// param-1 should be overridden by control
	if params["param-1"] != "Control Override" {
		t.Errorf("expected param-1 to be 'Control Override', got %q", params["param-1"])
	}

	// param-2 should come from catalog
	if params["param-2"] != "catalog-value" {
		t.Errorf("expected param-2 to be 'catalog-value', got %q", params["param-2"])
	}

	// param-3 should come from control
	if params["param-3"] != "Control Only" {
		t.Errorf("expected param-3 to be 'Control Only', got %q", params["param-3"])
	}

	// Should have exactly 3 params
	if len(params) != 3 {
		t.Errorf("expected 3 params, got %d", len(params))
	}
}

// Test 9: CollectParams prefers Label over Values
func TestCollectParams_PrefersLabel(t *testing.T) {
	catalogParams := []oscalTypes.Parameter{
		{
			ID:     "param-1",
			Label:  "Preferred Label",
			Values: &[]string{"fallback-value"},
		},
	}

	catalog := &oscalTypes.Catalog{
		UUID:   "test-catalog",
		Params: &catalogParams,
	}

	ctrl := oscalTypes.Control{
		ID:    "ac-9",
		Title: "Previous Logon Notification",
	}

	params := CollectParams(catalog, ctrl)

	if params["param-1"] != "Preferred Label" {
		t.Errorf("expected param-1 to be 'Preferred Label', got %q", params["param-1"])
	}
}

// Test 10: Round-trip preserves key fields
func TestRoundTrip_PreservesFields(t *testing.T) {
	// Start with a go-oscal Control
	parts := []oscalTypes.Part{
		{Name: "statement", Prose: "Original control statement."},
	}
	originalCtrl := oscalTypes.Control{
		ID:    "ac-10",
		Title: "Concurrent Session Control",
		Parts: &parts,
	}

	// Convert to ControlItem
	item := ControlToItem(originalCtrl, "ac", nil)

	// Convert back to Control
	roundTripCtrl := ItemToControl(item)

	// Verify key fields preserved
	if roundTripCtrl.ID != originalCtrl.ID {
		t.Errorf("ID not preserved: expected %q, got %q", originalCtrl.ID, roundTripCtrl.ID)
	}
	if roundTripCtrl.Title != originalCtrl.Title {
		t.Errorf("Title not preserved: expected %q, got %q", originalCtrl.Title, roundTripCtrl.Title)
	}

	if roundTripCtrl.Parts == nil || len(*roundTripCtrl.Parts) == 0 {
		t.Fatal("Parts not preserved")
	}

	roundTripPart := (*roundTripCtrl.Parts)[0]
	originalPart := (*originalCtrl.Parts)[0]
	if roundTripPart.Prose != originalPart.Prose {
		t.Errorf("Prose not preserved: expected %q, got %q", originalPart.Prose, roundTripPart.Prose)
	}
}

// Test 11: CollectParams handles nil catalog gracefully
func TestCollectParams_NilCatalog(t *testing.T) {
	controlParams := []oscalTypes.Parameter{
		{ID: "param-1", Label: "Control Param"},
	}

	ctrl := oscalTypes.Control{
		ID:     "ac-11",
		Title:  "Device Lock",
		Params: &controlParams,
	}

	params := CollectParams(nil, ctrl)

	if len(params) != 1 {
		t.Fatalf("expected 1 param, got %d", len(params))
	}

	if params["param-1"] != "Control Param" {
		t.Errorf("expected param-1 to be 'Control Param', got %q", params["param-1"])
	}
}

// Test 12: extractStatementProse handles deeply nested parts
func TestExtractStatementProse_DeeplyNested(t *testing.T) {
	level3 := []oscalTypes.Part{
		{Name: "item", Prose: "Level 3 requirement."},
	}
	level2 := []oscalTypes.Part{
		{Name: "item", Prose: "Level 2 requirement.", Parts: &level3},
	}
	level1 := []oscalTypes.Part{
		{
			Name:  "statement",
			Prose: "Level 1 requirement.",
			Parts: &level2,
		},
	}

	ctrl := oscalTypes.Control{
		ID:    "ac-12",
		Title: "Multi-level Control",
		Parts: &level1,
	}

	text := extractStatementProse(ctrl)

	expected := "Level 1 requirement.\nLevel 2 requirement.\nLevel 3 requirement."
	if text != expected {
		t.Errorf("expected:\n%q\ngot:\n%q", expected, text)
	}
}

// Test 13: ItemToControl handles empty text gracefully
func TestItemToControl_EmptyText(t *testing.T) {
	item := ControlItem{
		ID:    "ac-13",
		Title: "Supervision and Review - Access Control",
		Text:  "",
		Props: make(map[string]string),
	}

	ctrl := ItemToControl(item)

	if ctrl.Parts != nil {
		t.Errorf("expected Parts to be nil for empty text, got %d parts", len(*ctrl.Parts))
	}
}

// Test 14: ItemToControl handles empty props gracefully
func TestItemToControl_EmptyProps(t *testing.T) {
	item := ControlItem{
		ID:    "ac-14",
		Title: "Permitted Actions Without Identification",
		Text:  "Some text.",
		Props: make(map[string]string),
	}

	ctrl := ItemToControl(item)

	if ctrl.Props != nil {
		t.Errorf("expected Props to be nil for empty props map, got %d properties", len(*ctrl.Props))
	}
}
