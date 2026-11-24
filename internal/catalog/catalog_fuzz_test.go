package catalog_test

import (
	"encoding/json"
	"testing"

	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

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
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
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

func FuzzValidateTree(f *testing.F) {
	f.Add(`{"Items":[{"ID":"ac-1"},{"ID":"ac-2"}]}`)
	f.Add(`{"Items":[{"ID":"ac-1"},{"ID":"ac-1"}]}`)
	f.Add(`{"Items":[{"ID":"x","ParentID":"x"}]}`)
	f.Add(`{"Containers":[{"Key":"g1"},{"Key":"g1.1","ParentKey":"g1"}],"Items":[{"ID":"a","ContainerKey":"g1.1"}]}`)
	f.Add(`{"Containers":[{"Key":"g1","ParentKey":"g1"}],"Items":[{"ID":"a","ContainerKey":"g9"}]}`)
	f.Add(`{}`)
	f.Add(`{"Items":[{"ID":""}]}`)

	f.Fuzz(func(t *testing.T, data string) {
		var tree oscal.Tree
		if err := json.Unmarshal([]byte(data), &tree); err != nil {
			return // skip invalid JSON
		}
		// Must never panic.
		_ = catalog.ExportValidateTree(&tree)
	})
}

func FuzzAdaptIngestionOutput(f *testing.F) {
	f.Add([]byte(`{"schema_version":"1.2","elements":[{"element_id":"e-1","type":"heading","content":"A","level":1,"parent_id":null},{"element_id":"e-2","type":"table","content":"| a |\n|---|\n| 1 |","level":null,"parent_id":"e-1"}]}`))
	f.Add([]byte(`{"schema_version":"2.0","elements":[]}`))
	f.Add([]byte(`{"schema_version":"1.0","elements":[{"element_id":"e-1","type":"heading","content":"A","level":100}]}`))
	f.Add([]byte(`{"schema_version":"1.0","elements":[{"element_id":"e-1","type":"table","content":"| a \\| |\n|---|\n|\\|"}]}`))
	f.Add([]byte(`{"schema_version":"1.0","elements":[{"element_id":"e-1","type":"paragraph","content":"x","parent_id":"e-1"}]}`))
	f.Add([]byte(`{"schema_version":"99999999999999999999.0","elements":[[[[[[]]]]]]}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		doc, meta, err := catalog.AdaptIngestionOutput(data)
		if err != nil {
			return
		}
		if !catalog.IsIngestionOutput(data) {
			t.Fatalf("accepted output is not detected as ingestion output")
		}
		if meta.SchemaVersion == "" {
			t.Fatalf("accepted output has no schema version")
		}
		for i, s := range doc.Sections {
			if s.Level < 1 || s.Level > 100 {
				t.Fatalf("section %d has level %d", i, s.Level)
			}
		}
		for i, table := range doc.Tables {
			for j, row := range table.Rows {
				if len(row) != len(table.Headers) {
					t.Fatalf("table %d row %d has %d cells, header has %d", i, j, len(row), len(table.Headers))
				}
			}
		}
	})
}
