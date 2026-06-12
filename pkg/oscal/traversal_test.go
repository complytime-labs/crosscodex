package oscal

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"
)

// loadTestCatalog loads the minimal_catalog.json test fixture
func loadTestCatalog(t *testing.T) *oscalTypes.Catalog {
	t.Helper()

	data, err := os.ReadFile("testdata/minimal_catalog.json")
	if err != nil {
		t.Fatalf("failed to read test catalog: %v", err)
	}

	var schema oscalTypes.OscalCompleteSchema
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("failed to unmarshal test catalog: %v", err)
	}

	if schema.Catalog == nil {
		t.Fatal("test catalog is nil")
	}

	return schema.Catalog
}

func TestWalkControls_VisitsAllControls(t *testing.T) {
	catalog := loadTestCatalog(t)

	visited := make(map[string]bool)
	visitor := func(ctrl oscalTypes.Control, groupID string, depth int) {
		visited[ctrl.ID] = true
	}

	WalkControls(catalog, visitor)

	// Verify all controls were visited
	expectedControls := []string{"ac-1", "ac-2", "ac-2.1", "ac-3"}
	for _, id := range expectedControls {
		if !visited[id] {
			t.Errorf("control %s was not visited", id)
		}
	}

	// Verify no extra controls were visited
	if len(visited) != len(expectedControls) {
		t.Errorf("expected %d controls visited, got %d", len(expectedControls), len(visited))
	}
}

func TestWalkControls_PassesCorrectGroupID(t *testing.T) {
	catalog := loadTestCatalog(t)

	groupIDs := make(map[string]string)
	visitor := func(ctrl oscalTypes.Control, groupID string, depth int) {
		groupIDs[ctrl.ID] = groupID
	}

	WalkControls(catalog, visitor)

	// All controls in the test catalog are in the "ac" group
	expectedGroupID := "ac"
	for _, controlID := range []string{"ac-1", "ac-2", "ac-2.1", "ac-3"} {
		if groupIDs[controlID] != expectedGroupID {
			t.Errorf("control %s: expected groupID %q, got %q",
				controlID, expectedGroupID, groupIDs[controlID])
		}
	}
}

func TestWalkControls_IncrementsDepthForNestedControls(t *testing.T) {
	catalog := loadTestCatalog(t)

	depths := make(map[string]int)
	visitor := func(ctrl oscalTypes.Control, groupID string, depth int) {
		depths[ctrl.ID] = depth
	}

	WalkControls(catalog, visitor)

	// Top-level controls should have depth 0
	for _, controlID := range []string{"ac-1", "ac-2", "ac-3"} {
		if depths[controlID] != 0 {
			t.Errorf("control %s: expected depth 0, got %d", controlID, depths[controlID])
		}
	}

	// Sub-control (enhancement) should have depth 1
	if depths["ac-2.1"] != 1 {
		t.Errorf("control ac-2.1: expected depth 1, got %d", depths["ac-2.1"])
	}
}

func TestWalkControls_HandlesNilCatalog(t *testing.T) {
	// Should not panic
	WalkControls(nil, func(ctrl oscalTypes.Control, groupID string, depth int) {
		t.Error("visitor should not be called for nil catalog")
	})
}

func TestWalkControls_HandlesEmptyCatalog(t *testing.T) {
	catalog := &oscalTypes.Catalog{}

	visited := false
	visitor := func(ctrl oscalTypes.Control, groupID string, depth int) {
		visited = true
	}

	WalkControls(catalog, visitor)

	if visited {
		t.Error("visitor should not be called for empty catalog")
	}
}

func TestFindControlInSlice_FindsControlByExactID(t *testing.T) {
	items := []ControlItem{
		{ID: "ac-1", Title: "Access Control Policy"},
		{ID: "ac-2", Title: "Account Management"},
		{ID: "ac-3", Title: "Access Enforcement"},
	}

	item, err := FindControlInSlice(items, "ac-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if item == nil {
		t.Fatal("expected non-nil item")
	}

	if item.ID != "ac-2" {
		t.Errorf("expected ID ac-2, got %s", item.ID)
	}

	if item.Title != "Account Management" {
		t.Errorf("expected title 'Account Management', got %s", item.Title)
	}

	// Verify we got a pointer to the actual item in the slice
	item.Title = "Modified"
	if items[1].Title != "Modified" {
		t.Error("expected to get pointer to slice element, got a copy")
	}
}

func TestFindControlInSlice_ReturnsErrControlNotFoundForMissingID(t *testing.T) {
	items := []ControlItem{
		{ID: "ac-1", Title: "Access Control Policy"},
		{ID: "ac-2", Title: "Account Management"},
	}

	item, err := FindControlInSlice(items, "ac-99")

	if item != nil {
		t.Error("expected nil item for missing control")
	}

	if err == nil {
		t.Fatal("expected error for missing control")
	}

	if !errors.Is(err, ErrControlNotFound) {
		t.Errorf("expected ErrControlNotFound, got %v", err)
	}

	// Verify error message includes the control ID
	expectedMsg := "ac-99"
	if !contains(err.Error(), expectedMsg) {
		t.Errorf("error message should include control ID %q, got: %s", expectedMsg, err.Error())
	}
}

func TestFindControlInSlice_RejectsPartialMatches(t *testing.T) {
	items := []ControlItem{
		{ID: "ac-1", Title: "Access Control Policy"},
		{ID: "ac-10", Title: "Concurrent Session Control"},
	}

	// "ac" should not match "ac-1" or "ac-10"
	item, err := FindControlInSlice(items, "ac")
	if item != nil {
		t.Errorf("expected nil item for partial match 'ac', got %+v", item)
	}
	if err == nil {
		t.Error("expected error for partial match 'ac'")
	}

	// "ac-1" should not match "ac-10"
	item, err = FindControlInSlice(items, "ac-1")
	if err != nil {
		t.Errorf("expected to find ac-1, got error: %v", err)
	}
	if item == nil || item.ID != "ac-1" {
		t.Error("expected exact match for ac-1")
	}
}

func TestFindControlInSlice_HandlesEmptySlice(t *testing.T) {
	items := []ControlItem{}

	item, err := FindControlInSlice(items, "ac-1")

	if item != nil {
		t.Error("expected nil item for empty slice")
	}

	if err == nil {
		t.Fatal("expected error for empty slice")
	}

	if !errors.Is(err, ErrControlNotFound) {
		t.Errorf("expected ErrControlNotFound, got %v", err)
	}
}

// Helper function to check if a string contains a substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	if len(substr) > len(s) {
		return false
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
