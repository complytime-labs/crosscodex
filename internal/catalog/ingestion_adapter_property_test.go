//go:build !integration

package catalog_test

import (
	"encoding/json"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
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

		It("maps every role element of a source tree to one item, in order, that BuildTree accepts", func() {
			const maxDepth = 4
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				groupLevels := rapid.IntRange(0, 2).Draw(t, "groupLevels")
				var elements []map[string]any
				var wantIDs []string
				var wantGroup []bool
				groups := 0
				// node renders one source node at depth (0 = top) under the
				// element parent (nil at the top), then its subtree. Groups
				// (depth < groupLevels <= 2) always sit above the depth limit,
				// so each gets at least one child.
				var node func(depth int, parent any)
				node = func(depth int, parent any) {
					group := depth < groupLevels
					minChildren, maxChildren := 0, 2
					if group {
						minChildren, maxChildren = 1, 3
					}
					if depth == maxDepth-1 {
						maxChildren = 0
					}
					n := rapid.IntRange(minChildren, maxChildren).Draw(t, "children")
					elementID := fmt.Sprintf("e-%03d", len(elements))
					id := fmt.Sprintf("N-%03d", len(wantIDs))
					e := map[string]any{"element_id": elementID, "type": "paragraph", "content": "Body " + id,
						"level": nil, "parent_id": parent, "page": nil, "style": map[string]any{},
						"id": id, "role": "control", "title": nil, "text": "Body " + id}
					if n > 0 {
						e["type"], e["level"] = "heading", depth+1
					}
					if group {
						e["role"], e["title"], e["text"] = "group", "Group "+id, nil
						groups++
					}
					elements = append(elements, e)
					wantIDs = append(wantIDs, id)
					wantGroup = append(wantGroup, group)
					if n > 0 && rapid.Bool().Draw(t, "idlessText") {
						elements = append(elements, map[string]any{"element_id": fmt.Sprintf("e-%03d", len(elements)),
							"type": "paragraph", "content": "Kept for older readers.", "level": nil,
							"parent_id": elementID, "page": nil, "style": map[string]any{}, "id": nil})
					}
					for range n {
						node(depth+1, elementID)
					}
				}
				for range rapid.IntRange(1, 3).Draw(t, "roots") {
					node(0, nil)
				}
				data, err := json.Marshal(map[string]any{"schema_version": "1.3", "elements": elements})
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}

				_, meta, err := catalog.AdaptIngestionOutput(data)
				if err != nil {
					t.Fatalf("valid output rejected: %v", err)
				}
				if len(meta.SourceItems) != len(wantIDs) {
					t.Fatalf("got %d items, want %d", len(meta.SourceItems), len(wantIDs))
				}
				for i, item := range meta.SourceItems {
					if item.ID != wantIDs[i] {
						t.Fatalf("item %d has ID %q, want %q", i, item.ID, wantIDs[i])
					}
					if wantGroup[i] != (item.Class == "") {
						t.Fatalf("item %d (%s) has class %q, want group=%v", i, item.ID, item.Class, wantGroup[i])
					}
				}
				tree, err := oscal.BuildTree(meta.SourceItems)
				if err != nil {
					t.Fatalf("BuildTree: %v", err)
				}
				if len(tree.Containers) != groups {
					t.Fatalf("got %d containers, want %d", len(tree.Containers), groups)
				}
			})
		})
	})
})
