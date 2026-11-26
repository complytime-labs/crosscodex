package catalog_test

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
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

// propertyName mirrors the adapter's property-name pattern.
var propertyName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

var contentHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func FuzzAdaptIngestionOutput(f *testing.F) {
	f.Add([]byte(`{"schema_version":"1.2","elements":[{"element_id":"e-1","type":"heading","content":"A","level":1,"parent_id":null},{"element_id":"e-2","type":"table","content":"| a |\n|---|\n| 1 |","level":null,"parent_id":"e-1"}]}`))
	f.Add([]byte(`{"schema_version":"2.0","elements":[]}`))
	f.Add([]byte(`{"schema_version":"1.0","elements":[{"element_id":"e-1","type":"heading","content":"A","level":100}]}`))
	f.Add([]byte(`{"schema_version":"1.0","elements":[{"element_id":"e-1","type":"table","content":"| a \\| |\n|---|\n|\\|"}]}`))
	f.Add([]byte(`{"schema_version":"1.0","elements":[{"element_id":"e-1","type":"paragraph","content":"x","parent_id":"e-1"}]}`))
	f.Add([]byte(`{"schema_version":"99999999999999999999.0","elements":[[[[[[]]]]]]}`))
	f.Add([]byte(`{"schema_version":"1.3","metadata":{"title":"T","source":{"catalog_version":"1.0"}},"elements":[{"element_id":"e-1","type":"heading","content":"G","level":1,"parent_id":null,"id":"G1","role":"group","title":"G","text":null},{"element_id":"e-2","type":"paragraph","content":"C","level":null,"parent_id":"e-1","id":"G1.1","role":"control","title":null,"text":"C","properties":{"level":"1"}}]}`))
	f.Add([]byte(`{"schema_version":"1.3","elements":[{"element_id":"e-1","type":"paragraph","content":"C","role":"control","title":null,"text":"C"}]}`))
	f.Add([]byte(`{"schema_version":"1.3","elements":[{"element_id":"e-1","type":"heading","content":"C","level":1,"id":"C1","role":"control","title":null,"text":"C"},{"element_id":"e-2","type":"heading","content":"G","level":2,"parent_id":"e-1","id":"G1","role":"group","title":"G","text":null},{"element_id":"e-3","type":"paragraph","content":"C","parent_id":"e-2","id":"C2","role":"control","title":null,"text":"C"}]}`))
	f.Add([]byte(`{"schema_version":"1.3","metadata":{"title":"T","source":{"catalog_version":"1.0","mapping":{"id":"m","sha256":"de791a43657db03a3a728c4ec1bc14b31d400c0cfe70958fbe7d958847a0ec36","mapping_version":1}},"html_profile":{"id":"p","sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}},"elements":[{"element_id":"e-1","type":"paragraph","content":"C","id":"C1","role":"control","title":null,"text":"C"}]}`))
	f.Add(deepControlChain(50))

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
		if !contentHashPattern.MatchString(meta.ContentHash) {
			t.Fatalf("accepted output has content hash %q, want 64 lowercase hex", meta.ContentHash)
		}
		if _, again, err := catalog.AdaptIngestionOutput(data); err != nil || again.ContentHash != meta.ContentHash {
			t.Fatalf("content hash is not stable across decodes: %q then %q (err %v)", meta.ContentHash, again.ContentHash, err)
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
		if len(meta.SourceItems) == 0 {
			return
		}
		seenIDs := make(map[string]bool, len(meta.SourceItems))
		repeatedID := false
		for _, item := range meta.SourceItems {
			if seenIDs[item.ID] {
				repeatedID = true
				break
			}
			seenIDs[item.ID] = true
		}
		// Repeated source IDs are BuildTree's to refuse; the adapter does not check them.
		if !repeatedID {
			if _, err := oscal.BuildTree(meta.SourceItems); err != nil {
				t.Fatalf("accepted source items do not build a tree: %v", err)
			}
		}
		for i, item := range meta.SourceItems {
			if item.ID == "" {
				t.Fatalf("source item %d has no ID", i)
			}
			if item.Class != oscal.ClassRequirement && strings.TrimSpace(item.Title) == "" {
				t.Fatalf("group item %d has a blank title", i)
			}
			if item.Title != "" && strings.TrimSpace(item.Title) == "" {
				t.Fatalf("source item %d has a whitespace-only title", i)
			}
			if item.Text != "" && strings.TrimSpace(item.Text) == "" {
				t.Fatalf("source item %d has whitespace-only text", i)
			}
			for name, value := range item.Props {
				if !propertyName.MatchString(name) || strings.TrimSpace(value) == "" {
					t.Fatalf("source item %d has property %q with a bad name or blank value", i, name)
				}
			}
		}
	})
}

// deepControlChain renders schema 1.3 output with n controls, each the parent
// of the next.
func deepControlChain(n int) []byte {
	elements := make([]map[string]any, n)
	for i := range elements {
		e := map[string]any{"element_id": fmt.Sprintf("e-%d", i), "type": "heading", "content": "C",
			"level": i + 1, "parent_id": nil, "id": fmt.Sprintf("C%d", i), "role": "control", "title": nil, "text": "C"}
		if i > 0 {
			e["parent_id"] = fmt.Sprintf("e-%d", i-1)
		}
		if i == n-1 {
			e["type"], e["level"] = "paragraph", nil
		}
		elements[i] = e
	}
	data, err := json.Marshal(map[string]any{"schema_version": "1.3", "elements": elements})
	if err != nil {
		panic(err)
	}
	return data
}
