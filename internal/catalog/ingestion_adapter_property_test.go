//go:build !integration

package catalog_test

import (
	"encoding/json"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/internal/catalog"
)

var _ = Describe("Property Specifications", Ordered, func() {
	Context("AdaptIngestionOutput", func() {
		It("never panics on arbitrary element lists", func() {
			anyValue := rapid.SampledFrom([]any{nil, "", " ", "e-1", "e-2", "heading", 0, 1, 101, -1, 1.5, true,
				[]any{}, map[string]any{}, "| a |\n|---|\n| 1 |", "| a | b |\n|---|"})
			keys := rapid.SampledFrom([]string{"element_id", "type", "content", "level", "parent_id", "page", "style", "extra"})
			types := rapid.SampledFrom([]any{"heading", "paragraph", "list_item", "table", "figure", nil, 3})
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				n := rapid.IntRange(0, 12).Draw(t, "n")
				elements := make([]map[string]any, n)
				for i := range elements {
					e := map[string]any{"type": types.Draw(t, "type")}
					for range rapid.IntRange(0, 6).Draw(t, "fields") {
						e[keys.Draw(t, "key")] = anyValue.Draw(t, "value")
					}
					elements[i] = e
				}
				version := rapid.SampledFrom([]any{"1.0", "1.2", "2.0", "1", "", nil, 1.0}).Draw(t, "version")
				data, err := json.Marshal(map[string]any{"schema_version": version, "elements": elements})
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				_, _, _ = catalog.AdaptIngestionOutput(data)
				_ = catalog.IsIngestionOutput(data)
			})
		})

		It("detects as ingestion output everything it accepts, whatever the key case", func() {
			versionKeys := rapid.SampledFrom([]string{"schema_version", "SCHEMA_VERSION", "Schema_Version"})
			elementsKeys := rapid.SampledFrom([]string{"elements", "Elements", "ELEMENTS"})
			elementKeys := rapid.SampledFrom([]string{"element_id", "Element_ID", "type", "Type",
				"content", "Content", "level", "Level", "parent_id", "Parent_Id"})
			values := rapid.SampledFrom([]any{nil, "e-1", "paragraph", "heading", "x", 1, 2})
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				n := rapid.IntRange(0, 4).Draw(t, "n")
				elements := make([]map[string]any, n)
				for i := range elements {
					e := map[string]any{"element_id": fmt.Sprintf("e-%d", i), "type": "paragraph", "content": "x"}
					for range rapid.IntRange(0, 4).Draw(t, "fields") {
						e[elementKeys.Draw(t, "key")] = values.Draw(t, "value")
					}
					elements[i] = e
				}
				top := map[string]any{}
				for range rapid.IntRange(0, 3).Draw(t, "versionKeys") {
					top[versionKeys.Draw(t, "versionKey")] = "1.0"
				}
				for range rapid.IntRange(0, 3).Draw(t, "elementsKeys") {
					top[elementsKeys.Draw(t, "elementsKey")] = elements
				}
				data, err := json.Marshal(top)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				if _, _, err := catalog.AdaptIngestionOutput(data); err == nil && !catalog.IsIngestionOutput(data) {
					t.Fatalf("accepted output is not detected as ingestion output: %s", data)
				}
			})
		})

		It("never panics on arbitrary bytes", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				data := rapid.SliceOf(rapid.Byte()).Draw(t, "data")
				_, _, _ = catalog.AdaptIngestionOutput(data)
				_ = catalog.IsIngestionOutput(data)
			})
		})

		It("preserves heading order and levels for valid output", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				n := rapid.IntRange(1, 30).Draw(t, "n")
				elements := make([]map[string]any, n)
				var headingIDs []string
				var wantTitles []string
				var wantLevels []int
				for i := range elements {
					id := fmt.Sprintf("e-%03d", i)
					var parent any
					if len(headingIDs) > 0 && rapid.Bool().Draw(t, "hasParent") {
						parent = rapid.SampledFrom(headingIDs).Draw(t, "parent")
					}
					content := rapid.StringMatching(`[A-Za-z0-9][A-Za-z0-9 .()-]{0,20}`).Draw(t, "content")
					e := map[string]any{"element_id": id, "content": content, "parent_id": parent,
						"level": nil, "page": nil, "style": map[string]any{}}
					switch kind := rapid.IntRange(0, 3).Draw(t, "kind"); kind {
					case 0:
						level := rapid.IntRange(1, 100).Draw(t, "level")
						e["type"], e["level"] = "heading", level
						headingIDs = append(headingIDs, id)
						wantTitles = append(wantTitles, content)
						wantLevels = append(wantLevels, level)
					case 1:
						e["type"] = "paragraph"
					case 2:
						e["type"] = "list_item"
					default:
						e["type"], e["content"] = "table", "| H |\n|---|\n| "+content+" |"
					}
					elements[i] = e
				}
				data, err := json.Marshal(map[string]any{"schema_version": "1.2", "elements": elements})
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				doc, _, err := catalog.AdaptIngestionOutput(data)
				if err != nil {
					t.Fatalf("valid output rejected: %v", err)
				}
				if len(doc.Sections) != len(wantTitles) {
					t.Fatalf("got %d sections, want %d", len(doc.Sections), len(wantTitles))
				}
				for i, s := range doc.Sections {
					if s.Title != wantTitles[i] || s.Level != wantLevels[i] {
						t.Fatalf("section %d = (%q, %d), want (%q, %d)", i, s.Title, s.Level, wantTitles[i], wantLevels[i])
					}
				}
			})
		})
	})
})
