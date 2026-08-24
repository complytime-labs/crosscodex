//go:build integration

package main

import (
	"context"
	"os"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

const e2eCatalogFixture = "testdata/oscal/e2e-minimal-catalog.json"

// TestE2EFixtureParses locks the fixture's shape: the graph/relational
// assertions in the Tier 0 test depend on exactly these items existing.
func TestE2EFixtureParses(t *testing.T) {
	f, err := os.Open(e2eCatalogFixture)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	items, err := oscal.NewParser("").Parse(context.Background(), f)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}

	byID := map[string]oscal.ControlItem{}
	for _, it := range items {
		byID[it.ID] = it
	}
	if len(items) != 3 {
		t.Fatalf("item count: got %d, want 3 (%v)", len(items), byID)
	}
	parent, ok := byID["ac-2"]
	if !ok || parent.Class != oscal.ClassSection || parent.ParentID != "" {
		t.Errorf("ac-2: got %+v, want ClassSection with no parent", parent)
	}
	for _, child := range []string{"ac-2.a", "ac-2.b"} {
		c, ok := byID[child]
		if !ok || c.ParentID != "ac-2" || c.Class != oscal.ClassRequirement {
			t.Errorf("%s: got %+v, want ClassRequirement child of ac-2", child, c)
		}
	}
	if byID["ac-2.a"].Text != byID["ac-2.b"].Text || byID["ac-2.a"].Text == "" {
		t.Errorf("children must share identical non-empty Text to form a requires candidate pair")
	}
}
