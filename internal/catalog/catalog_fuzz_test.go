package catalog_test

import (
	"encoding/json"
	"testing"

	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

func FuzzIsOSCALJSON(f *testing.F) {
	f.Add([]byte(`{"catalog":{"uuid":"abc"}}`))
	f.Add([]byte(`{"document":{"title":"test"}}`))
	f.Add([]byte(`{invalid`))
	f.Add([]byte(``))
	f.Add([]byte(`null`))
	f.Add([]byte(`[1,2,3]`))
	f.Add([]byte(`{"catalog": null}`))
	f.Add([]byte(`{"CATALOG":{}}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must never panic.
		_ = catalog.ExportIsOSCALJSON(data)
	})
}

func FuzzGenerateCatalogID(f *testing.F) {
	f.Add("hash1", "tenant-1")
	f.Add("", "")
	f.Add("abc", "xyz")
	f.Add("sha256:abcdef1234567890", "org-production")

	f.Fuzz(func(t *testing.T, contentHash, tenantID string) {
		result := catalog.ExportGenerateCatalogID(contentHash, tenantID)
		if len(result) != 16 {
			t.Fatalf("expected 16 chars, got %d: %q", len(result), result)
		}
		// Must be valid hex.
		for _, c := range result {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Fatalf("non-hex char %c in result %q", c, result)
			}
		}
		// Determinism.
		result2 := catalog.ExportGenerateCatalogID(contentHash, tenantID)
		if result != result2 {
			t.Fatalf("non-deterministic: %q != %q", result, result2)
		}
	})
}

func FuzzValidateItems(f *testing.F) {
	f.Add(`[{"ID":"ac-1"},{"ID":"ac-2"}]`)
	f.Add(`[{"ID":"ac-1"},{"ID":"ac-1"}]`)
	f.Add(`[{"ID":"x","ParentID":"x"}]`)
	f.Add(`[]`)
	f.Add(`[{"ID":""}]`)

	f.Fuzz(func(t *testing.T, data string) {
		var items []oscal.ControlItem
		if err := json.Unmarshal([]byte(data), &items); err != nil {
			return // skip invalid JSON
		}
		// Must never panic.
		_ = catalog.ExportValidateItems(items)
	})
}
