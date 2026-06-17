package catalog_test

import (
	"encoding/json"
	"regexp"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"pgregory.net/rapid"

	crosscodexv1 "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
)

var hexPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

var _ = Describe("Property Specifications", Ordered, func() {

	Context("isOSCALJSON — fail-closed on invalid JSON", func() {
		It("never panics on arbitrary bytes", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				data := rapid.SliceOf(rapid.Byte()).Draw(t, "data")
				// Must never panic — result does not matter.
				_ = catalog.ExportIsOSCALJSON(data)
			})
		})

		It("returns true for valid JSON with catalog key, false without", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				hasCatalog := rapid.Bool().Draw(t, "hasCatalog")
				obj := make(map[string]interface{})
				// Add 0-3 random keys.
				nKeys := rapid.IntRange(0, 3).Draw(t, "nKeys")
				for i := 0; i < nKeys; i++ {
					key := rapid.StringMatching(`^[a-z]{1,8}$`).Draw(t, "key")
					if key == "catalog" {
						continue // don't accidentally add catalog key
					}
					obj[key] = rapid.String().Draw(t, "val")
				}
				if hasCatalog {
					obj["catalog"] = map[string]interface{}{
						"uuid": rapid.String().Draw(t, "uuid"),
					}
				}
				data, err := json.Marshal(obj)
				if err != nil {
					t.Fatalf("json.Marshal: %v", err)
				}
				result := catalog.ExportIsOSCALJSON(data)
				if hasCatalog && !result {
					t.Fatalf("expected true for JSON with catalog key, got false: %s", data)
				}
				if !hasCatalog && result {
					t.Fatalf("expected false for JSON without catalog key, got true: %s", data)
				}
			})
		})
	})

	Context("validateItems — duplicate detection", func() {
		It("returns error when at least two items share the same ID", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				dupID := rapid.StringMatching(`^[a-z][a-z0-9-]{1,10}$`).Draw(t, "dupID")
				// Build a slice with at least two items sharing dupID.
				nExtra := rapid.IntRange(0, 5).Draw(t, "nExtra")
				items := make([]oscal.ControlItem, 0, nExtra+2)
				items = append(items, oscal.ControlItem{ID: dupID})
				// Add extras with unique IDs.
				for i := 0; i < nExtra; i++ {
					extraID := rapid.StringMatching(`^[a-z][a-z0-9-]{1,10}$`).Draw(t, "extraID")
					if extraID == dupID {
						continue
					}
					items = append(items, oscal.ControlItem{ID: extraID})
				}
				// Add the duplicate.
				items = append(items, oscal.ControlItem{ID: dupID})

				err := catalog.ExportValidateItems(items)
				if err == nil {
					t.Fatalf("expected duplicate error for ID %q, got nil", dupID)
				}
			})
		})

		It("returns nil for all unique non-empty IDs with no self-reference", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				n := rapid.IntRange(0, 10).Draw(t, "n")
				seen := make(map[string]bool)
				items := make([]oscal.ControlItem, 0, n)
				for i := 0; i < n; i++ {
					id := rapid.StringMatching(`^[a-z][a-z0-9-]{1,10}$`).Draw(t, "id")
					if seen[id] {
						continue // skip to keep unique
					}
					seen[id] = true
					// ParentID must differ from ID. Use empty string.
					items = append(items, oscal.ControlItem{ID: id, ParentID: ""})
				}
				err := catalog.ExportValidateItems(items)
				if err != nil {
					t.Fatalf("expected nil error for unique IDs, got: %v", err)
				}
			})
		})
	})

	Context("validateItems — self-reference detection", func() {
		It("returns error when ParentID equals ID and both are non-empty", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				id := rapid.StringMatching(`^[a-z][a-z0-9-]{1,10}$`).Draw(t, "id")
				items := []oscal.ControlItem{{ID: id, ParentID: id}}
				err := catalog.ExportValidateItems(items)
				if err == nil {
					t.Fatalf("expected self-reference error for ID %q, got nil", id)
				}
			})
		})
	})

	Context("generateCatalogID — determinism and format", func() {
		It("produces the same output for the same inputs", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				hash := rapid.String().Draw(t, "contentHash")
				tenant := rapid.String().Draw(t, "tenantID")
				a := catalog.ExportGenerateCatalogID(hash, tenant)
				b := catalog.ExportGenerateCatalogID(hash, tenant)
				if a != b {
					t.Fatalf("non-deterministic: %q != %q for (%q, %q)", a, b, hash, tenant)
				}
			})
		})

		It("always produces exactly 16 lowercase hex characters", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				hash := rapid.String().Draw(t, "contentHash")
				tenant := rapid.String().Draw(t, "tenantID")
				result := catalog.ExportGenerateCatalogID(hash, tenant)
				if !hexPattern.MatchString(result) {
					t.Fatalf("expected 16 hex chars matching ^[0-9a-f]{16}$, got %q", result)
				}
			})
		})
	})

	Context("EffectiveLimit — clamping", func() {
		It("clamps values into [1, 1000] with default 50 for non-positive", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				limit := rapid.IntRange(-100, 2000).Draw(t, "limit")
				result := catalog.ListOptions{Limit: limit}.EffectiveLimit()
				switch {
				case limit <= 0:
					if result != 50 {
						t.Fatalf("expected 50 for limit %d, got %d", limit, result)
					}
				case limit > 1000:
					if result != 1000 {
						t.Fatalf("expected 1000 for limit %d, got %d", limit, result)
					}
				default:
					if result != limit {
						t.Fatalf("expected %d for limit %d, got %d", limit, limit, result)
					}
				}
			})
		})

		It("always returns a value in [1, 1000]", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				limit := rapid.IntRange(-100, 2000).Draw(t, "limit")
				result := catalog.ListOptions{Limit: limit}.EffectiveLimit()
				if result < 1 || result > 1000 {
					t.Fatalf("result %d out of [1, 1000] for limit %d", result, limit)
				}
			})
		})
	})

	Context("mergeResults — no duplicates and FT prefix", func() {
		It("never contains duplicate ControlIDs", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				nFT := rapid.IntRange(0, 5).Draw(t, "nFT")
				ftRecords := make([]catalog.ExportControlRecord, nFT)
				for i := 0; i < nFT; i++ {
					ftRecords[i] = catalog.ExportControlRecord{
						ControlID: rapid.StringMatching(`^[a-z][a-z0-9-]{1,10}$`).Draw(t, "ftID"),
					}
				}

				nSem := rapid.IntRange(0, 5).Draw(t, "nSem")
				semResults := make([]vectordb.SimilarityResult, nSem)
				for i := 0; i < nSem; i++ {
					semResults[i] = vectordb.SimilarityResult{
						ControlID:  rapid.StringMatching(`^[a-z][a-z0-9-]{1,10}$`).Draw(t, "semID"),
						Similarity: 0.5,
					}
				}

				merged := catalog.ExportMergeResults(ftRecords, semResults)
				seen := make(map[string]bool)
				for _, rec := range merged {
					if seen[rec.ControlID] {
						t.Fatalf("duplicate ControlID in merged results: %q", rec.ControlID)
					}
					seen[rec.ControlID] = true
				}
			})
		})

		It("preserves FT records as prefix of output", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				nFT := rapid.IntRange(1, 5).Draw(t, "nFT")
				ftRecords := make([]catalog.ExportControlRecord, nFT)
				for i := 0; i < nFT; i++ {
					ftRecords[i] = catalog.ExportControlRecord{
						ControlID: rapid.StringMatching(`^[a-z][a-z0-9-]{1,10}$`).Draw(t, "ftID"),
						Title:     rapid.String().Draw(t, "ftTitle"),
					}
				}

				nSem := rapid.IntRange(0, 5).Draw(t, "nSem")
				semResults := make([]vectordb.SimilarityResult, nSem)
				for i := 0; i < nSem; i++ {
					semResults[i] = vectordb.SimilarityResult{
						ControlID:  rapid.StringMatching(`^[a-z][a-z0-9-]{1,10}$`).Draw(t, "semID"),
						Similarity: 0.5,
					}
				}

				// Compute deduplicated FT prefix (first-occurrence order)
				dedupFT := make([]catalog.ExportControlRecord, 0, len(ftRecords))
				seenFT := make(map[string]bool, len(ftRecords))
				for _, ft := range ftRecords {
					if !seenFT[ft.ControlID] {
						seenFT[ft.ControlID] = true
						dedupFT = append(dedupFT, ft)
					}
				}

				merged := catalog.ExportMergeResults(ftRecords, semResults)
				if len(merged) < len(dedupFT) {
					t.Fatalf("merged has fewer records (%d) than deduplicated FT input (%d)", len(merged), len(dedupFT))
				}
				for i, ft := range dedupFT {
					if merged[i].ControlID != ft.ControlID {
						t.Fatalf("merged[%d].ControlID = %q, want %q", i, merged[i].ControlID, ft.ControlID)
					}
				}
			})
		})

		It("returns deduplicated ftRecords when semanticResults is empty", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				nFT := rapid.IntRange(0, 5).Draw(t, "nFT")
				ftRecords := make([]catalog.ExportControlRecord, nFT)
				for i := 0; i < nFT; i++ {
					ftRecords[i] = catalog.ExportControlRecord{
						ControlID: rapid.StringMatching(`^[a-z][a-z0-9-]{1,10}$`).Draw(t, "ftID"),
					}
				}

				// Compute expected deduplicated result (first-occurrence order)
				expected := make([]catalog.ExportControlRecord, 0, len(ftRecords))
				seen := make(map[string]bool, len(ftRecords))
				for _, ft := range ftRecords {
					if !seen[ft.ControlID] {
						seen[ft.ControlID] = true
						expected = append(expected, ft)
					}
				}

				merged := catalog.ExportMergeResults(ftRecords, nil)
				Expect(merged).To(Equal(expected))
			})
		})
	})

	Context("catalogRecordToProto / controlRecordToProto — nil safety and field preservation", func() {
		It("returns nil for nil CatalogRecord input", func() {
			result := catalog.ExportCatalogRecordToProto(nil)
			Expect(result).To(BeNil())
		})

		It("returns nil for nil ControlRecord input", func() {
			result := catalog.ExportControlRecordToProto(nil)
			Expect(result).To(BeNil())
		})

		It("preserves CatalogRecord fields in proto conversion", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				rec := &catalog.ExportCatalogRecord{
					CatalogID: rapid.StringMatching(`^[a-z][a-z0-9-]{1,10}$`).Draw(t, "catalogID"),
					TenantID:  rapid.StringMatching(`^[a-z][a-z0-9-]{1,10}$`).Draw(t, "tenantID"),
					Name:      rapid.String().Draw(t, "name"),
					Version:   rapid.String().Draw(t, "version"),
					CreatedAt: time.Now().UTC(),
				}
				proto := catalog.ExportCatalogRecordToProto(rec)
				if proto == nil {
					t.Fatal("expected non-nil proto for non-nil input")
				}
				if proto.GetCatalogId() != rec.CatalogID {
					t.Fatalf("CatalogId: got %q, want %q", proto.GetCatalogId(), rec.CatalogID)
				}
				if proto.GetName() != rec.Name {
					t.Fatalf("Name: got %q, want %q", proto.GetName(), rec.Name)
				}
				if proto.GetVersion() != rec.Version {
					t.Fatalf("Version: got %q, want %q", proto.GetVersion(), rec.Version)
				}
				if proto.GetTenantContext() == nil || proto.GetTenantContext().GetTenantId() != rec.TenantID {
					t.Fatalf("TenantID: got %q, want %q",
						proto.GetTenantContext().GetTenantId(), rec.TenantID)
				}
			})
		})

		It("preserves ControlRecord fields in proto conversion", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				rec := &catalog.ExportControlRecord{
					ControlID:  rapid.StringMatching(`^[a-z][a-z0-9-]{1,10}$`).Draw(t, "controlID"),
					CatalogID:  rapid.StringMatching(`^[a-z][a-z0-9-]{1,10}$`).Draw(t, "catalogID"),
					Identifier: rapid.StringMatching(`^[a-z][a-z0-9-]{1,10}$`).Draw(t, "identifier"),
					Title:      rapid.String().Draw(t, "title"),
					Statement:  rapid.String().Draw(t, "statement"),
					CreatedAt:  time.Now().UTC(),
				}
				proto := catalog.ExportControlRecordToProto(rec)
				if proto == nil {
					t.Fatal("expected non-nil proto for non-nil input")
				}
				if proto.GetControlId() != rec.ControlID {
					t.Fatalf("ControlId: got %q, want %q", proto.GetControlId(), rec.ControlID)
				}
				if proto.GetCatalogId() != rec.CatalogID {
					t.Fatalf("CatalogId: got %q, want %q", proto.GetCatalogId(), rec.CatalogID)
				}
				if proto.GetIdentifier() != rec.Identifier {
					t.Fatalf("Identifier: got %q, want %q", proto.GetIdentifier(), rec.Identifier)
				}
				if proto.GetTitle() != rec.Title {
					t.Fatalf("Title: got %q, want %q", proto.GetTitle(), rec.Title)
				}
				if proto.GetStatement() != rec.Statement {
					t.Fatalf("Statement: got %q, want %q", proto.GetStatement(), rec.Statement)
				}
			})
		})
	})
})

// Ensure proto import is used (compile guard).
var _ *crosscodexv1.Catalog
