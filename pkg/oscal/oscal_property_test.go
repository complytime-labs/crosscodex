package oscal_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/defenseunicorns/go-oscal/src/pkg/validation"
	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

// Suite bootstrap lives in oscal_bdd_test.go (TestOscalBDD).
// This file only registers Describe nodes; Ginkgo collects them automatically.

var _ = Describe("Property Specifications", Ordered, func() {
	Context("DetectCatalogJSON — fail-closed on invalid JSON", func() {
		It("never panics on arbitrary bytes", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				data := rapid.SliceOf(rapid.Byte()).Draw(t, "data")
				// Must never panic — result does not matter.
				_ = oscal.DetectCatalogJSON(data).MetadataString("title")
			})
		})

		It("returns true for a JSON object with the catalog key, false without", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				hasCatalog := rapid.Bool().Draw(t, "hasCatalog")
				obj := make(map[string]interface{})
				nKeys := rapid.IntRange(0, 3).Draw(t, "nKeys")
				for i := 0; i < nKeys; i++ {
					key := rapid.StringMatching(`^[a-z]{1,8}$`).Draw(t, "key")
					if key == oscal.CatalogRootKey {
						continue // don't accidentally add the catalog key
					}
					obj[key] = rapid.String().Draw(t, "val")
				}
				if hasCatalog {
					obj[oscal.CatalogRootKey] = map[string]interface{}{
						"uuid": rapid.String().Draw(t, "uuid"),
					}
				}
				data, err := json.Marshal(obj)
				if err != nil {
					t.Fatalf("json.Marshal: %v", err)
				}
				if got := oscal.DetectCatalogJSON(data).IsCatalog; got != hasCatalog {
					t.Fatalf("IsCatalog = %v, want %v for %s", got, hasCatalog, data)
				}
			})
		})

		It("IsModelJSON is true exactly when a JSON object has a model root key, and covers every catalog", func() {
			models := oscal.ModelRootKeys()
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				obj := map[string]any{}
				nKeys := rapid.IntRange(0, 3).Draw(t, "nKeys")
				for i := 0; i < nKeys; i++ {
					// Hyphenated lowercase keys, the shape of OSCAL model names.
					key := rapid.StringMatching(`^[a-z]{1,8}(-[a-z]{1,8})?$`).Draw(t, "key")
					if slices.Contains(models, key) {
						continue
					}
					obj[key] = rapid.String().Draw(t, "val")
				}
				hasModel := rapid.Bool().Draw(t, "hasModel")
				if hasModel {
					obj[rapid.SampledFrom(models).Draw(t, "model")] = map[string]any{}
				}
				data, err := json.Marshal(obj)
				if err != nil {
					t.Fatalf("json.Marshal: %v", err)
				}
				if got := oscal.IsModelJSON(data); got != hasModel {
					t.Fatalf("IsModelJSON = %v, want %v for %s", got, hasModel, data)
				}
				if oscal.DetectCatalogJSON(data).IsCatalog && !oscal.IsModelJSON(data) {
					t.Fatalf("a catalog is not detected as an OSCAL model: %s", data)
				}
			})
		})

		It("returns any string metadata field unchanged", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				title := rapid.String().Draw(t, "title")
				data, err := json.Marshal(map[string]any{
					oscal.CatalogRootKey: map[string]any{oscal.MetadataKey: map[string]any{"title": title}},
				})
				if err != nil {
					t.Fatalf("json.Marshal: %v", err)
				}
				// rapid.String draws valid UTF-8, which round-trips through JSON.
				if got := oscal.DetectCatalogJSON(data).MetadataString("title"); got != title {
					t.Fatalf("MetadataString = %q, want %q", got, title)
				}
			})
		})
	})

	Context("UsableTitle", func() {
		It("returns either empty or the trimmed title", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				title := rapid.String().Draw(t, "title")
				identifier := rapid.String().Draw(t, "identifier")
				label := rapid.String().Draw(t, "label")
				got := oscal.UsableTitle(title, identifier, label)
				if got != "" && got != strings.TrimSpace(title) {
					t.Fatalf("UsableTitle(%q, %q, %q) = %q, want \"\" or %q", title, identifier, label, got, strings.TrimSpace(title))
				}
			})
		})

		It("rejects an identifier used as its own title", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				id := rapid.String().Filter(func(s string) bool {
					return strings.IndexFunc(s, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0
				}).Draw(t, "id")
				if got := oscal.UsableTitle(id, id, ""); got != "" {
					t.Fatalf("UsableTitle(%q, %q, \"\") = %q, want \"\"", id, id, got)
				}
			})
		})

		It("rejects a numbered identifier respelt with other separators and case", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				words := rapid.SliceOfN(rapid.StringMatching(`[a-zA-Z]{1,8}`), 1, 4).Draw(t, "words")
				number := rapid.StringMatching(`[0-9]{1,3}(\.[0-9]{1,3}){0,2}`).Draw(t, "number")
				identifier := strings.Join(append(words, number), "_")
				title := strings.ToUpper(strings.Join(words, " ")) + " " + number
				if got := oscal.UsableTitle(title, identifier, ""); got != "" {
					t.Fatalf("UsableTitle(%q, %q, \"\") = %q, want \"\"", title, identifier, got)
				}
			})
		})

		It("keeps a digit-free title that matches a slug identifier only by letters", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				words := rapid.SliceOfN(rapid.StringMatching(`[a-zA-Z]{1,8}`), 2, 4).Draw(t, "words")
				identifier := strings.ToLower(strings.Join(words, "_"))
				title := strings.Join(words, " ")
				if got := oscal.UsableTitle(title, identifier, ""); got != title {
					t.Fatalf("UsableTitle(%q, %q, \"\") = %q, want %q", title, identifier, got, title)
				}
			})
		})
	})

	Context("CleanProse — template resolution", func() {
		It("never leaves matched template pairs in output", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				// Generate text that may contain OSCAL template patterns
				text := rapid.StringMatching(`[a-zA-Z0-9 .,\n\{\}:_-]{0,200}`).Draw(t, "text")

				// Generate 0-5 parameter entries
				numParams := rapid.IntRange(0, 5).Draw(t, "numParams")
				params := make(map[string]string)
				for i := 0; i < numParams; i++ {
					key := rapid.StringMatching(`[a-z][a-z0-9_-]{0,15}`).Draw(t, "paramKey")
					val := rapid.StringMatching(`[a-zA-Z0-9 ]{1,20}`).Draw(t, "paramVal")
					params[key] = val
				}

				result := oscal.CleanProse(text, params)
				// CleanProse strips all {{...}} pairs (non-greedy match).
				// Unmatched }} without preceding {{ is literal text, not a template.
				Expect(result).NotTo(MatchRegexp(`\{\{.*?\}\}`))
			})
		})

		It("returns original text when no templates present", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				// Generate text guaranteed to have no braces
				text := rapid.StringMatching(`[a-zA-Z0-9 .,;:!?-]{0,100}`).Draw(t, "plainText")
				result := oscal.CleanProse(text, nil)
				Expect(result).To(Equal(text))
			})
		})
	})

	Context("CleanForEmbedding — idempotency", func() {
		It("is idempotent for arbitrary input", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				text := rapid.String().Draw(t, "text")
				once := oscal.CleanForEmbedding(text)
				twice := oscal.CleanForEmbedding(once)
				Expect(twice).To(Equal(once))
			})
		})

		It("never produces 3+ consecutive newlines", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				text := rapid.String().Draw(t, "text")
				result := oscal.CleanForEmbedding(text)
				Expect(result).NotTo(MatchRegexp(`\n{3,}`))
			})
		})
	})

	Context("Structure — print-artefact clean-up", func() {
		It("removes header lines from prose but never leaves an item blank", func() {
			opts := oscal.StructureOptions{PrintArtifactPatterns: examplePrintArtifactPatterns}
			structurer := oscal.NewStructurer(nil, nil)
			headers := []string{gpoHeader, "G:\\COMP\\INTEL\\NSA1947.XML", deFooter}
			prose := []string{"The agency shall review access.", "Obtain approval for PO 12345.", "Siehe Seite 3 von 12 im Anhang."}
			lines := append(append([]string{}, headers...), prose...)

			rapid.Check(GinkgoT(), func(t *rapid.T) {
				drawn := rapid.SliceOfN(rapid.SampledFrom(lines), 1, 8).Draw(t, "lines")
				items, err := structurer.Structure(context.Background(), oscal.StructuredDoc{RawText: strings.Join(drawn, "\n")}, opts)
				if err != nil {
					t.Fatalf("Structure: %v", err)
				}
				hasProse := false
				for _, l := range drawn {
					hasProse = hasProse || slices.Contains(prose, l)
				}
				for _, item := range items {
					if strings.TrimSpace(item.Text) == "" {
						t.Fatalf("item %s has blank text from lines %q", item.ID, drawn)
					}
					for _, h := range headers {
						if hasProse && slices.Contains(strings.Split(item.Text, "\n"), h) {
							t.Fatalf("item %s kept header line %q beside prose: %q", item.ID, h, item.Text)
						}
					}
				}
			})
		})
	})

	Context("DecomposeText — structural guarantees", func() {
		It("always returns at least one ControlItem", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				baseID := rapid.StringMatching(`[a-z]{2,4}-[0-9]{1,3}`).Draw(t, "baseID")
				text := rapid.String().Draw(t, "text")
				minWords := rapid.IntRange(1, 50).Draw(t, "minWords")

				items := oscal.DecomposeText(baseID, text, minWords)
				Expect(items).NotTo(BeEmpty())
			})
		})

		It("every item has non-nil Props", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				baseID := rapid.StringMatching(`[a-z]{2,4}-[0-9]{1,3}`).Draw(t, "baseID")
				text := rapid.String().Draw(t, "text")
				minWords := rapid.IntRange(1, 50).Draw(t, "minWords")

				items := oscal.DecomposeText(baseID, text, minWords)
				for i, item := range items {
					Expect(item.Props).NotTo(BeNil(), "item %d (%s) has nil Props", i, item.ID)
				}
			})
		})

		It("preserves baseID prefix in all item IDs", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				baseID := rapid.StringMatching(`[a-z]{2,4}-[0-9]{1,3}`).Draw(t, "baseID")
				text := rapid.StringMatching(`[a-zA-Z0-9 .,()\n]{10,200}`).Draw(t, "text")
				minWords := rapid.IntRange(1, 20).Draw(t, "minWords")

				items := oscal.DecomposeText(baseID, text, minWords)
				for i, item := range items {
					Expect(item.ID).To(HavePrefix(baseID),
						"item %d ID %q does not start with baseID %q", i, item.ID, baseID)
				}
			})
		})
	})

	Context("CollectParams — parameter rendering", func() {
		It("renders a label-only parameter as an assignment containing the label", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				label := rapid.StringMatching(`[a-zA-Z0-9][a-zA-Z0-9 ,-]{0,40}`).Draw(t, "label")
				ctrl := oscalTypes.Control{ID: "x-1", Params: &[]oscalTypes.Parameter{{ID: "p", Label: label}}}

				got := oscal.CollectParams(nil, ctrl)["p"]
				if !strings.HasPrefix(got, "[Assignment: ") || !strings.Contains(got, label) {
					t.Fatalf("label %q rendered as %q", label, got)
				}
			})
		})

		It("renders a set value verbatim, whatever the label", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				value := rapid.StringMatching(`[a-zA-Z0-9][a-zA-Z0-9 ,-]{0,40}`).Draw(t, "value")
				label := rapid.StringMatching(`[a-zA-Z0-9 ]{0,40}`).Draw(t, "label")
				ctrl := oscalTypes.Control{ID: "x-1", Params: &[]oscalTypes.Parameter{{ID: "p", Label: label, Values: &[]string{value}}}}

				if got := oscal.CollectParams(nil, ctrl)["p"]; got != value {
					t.Fatalf("value %q (label %q) rendered as %q", value, label, got)
				}
			})
		})
	})

	Context("wordCount — consistency with strings.Fields", func() {
		It("matches len(strings.Fields(s)) for arbitrary strings", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				s := rapid.String().Draw(t, "input")
				got := oscal.ExportWordCount(s)
				want := len(strings.Fields(s))
				Expect(got).To(Equal(want))
			})
		})

		It("returns zero for whitespace-only input", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				s := rapid.StringMatching(`[ \t\n\r]{0,50}`).Draw(t, "whitespace")
				got := oscal.ExportWordCount(s)
				Expect(got).To(Equal(0))
			})
		})
	})

	Context("BuildMappingCollection", func() {
		relationships := []string{"EQUIVALENT", "SUPERSET_OF", "SUBSET_OF", "NO_RELATIONSHIP", "CONTRIBUTES_TO", "COMPLEMENTS", "PARTIAL", "CONFLICTS_WITH"}

		drawExport := func(t *rapid.T) oscal.MappingExport {
			in := oscal.MappingExport{
				JobID:       "job",
				Stage:       rapid.SampledFrom([]string{"synthesized", "analyzer"}).Draw(t, "stage"),
				CompletedAt: time.Unix(rapid.Int64Range(0, 4_000_000_000).Draw(t, "completed"), 0),
				Source:      oscal.ExportCatalog{ID: "src", Name: rapid.String().Draw(t, "srcName"), Version: "1"},
				Target:      oscal.ExportCatalog{ID: "tgt", Name: rapid.String().Draw(t, "tgtName"), VersionDisplay: "v2"},
			}
			n := rapid.IntRange(1, 20).Draw(t, "rows")
			for i := range n {
				from, to := "src", "tgt"
				if rapid.Bool().Draw(t, "reverse") {
					from, to = to, from
				}
				in.Mappings = append(in.Mappings, oscal.ExportMapping{
					MappingID:       fmt.Sprintf("m-%d", i),
					SourceControlID: from + "/" + rapid.StringMatching(`[a-z0-9][a-z0-9.\-]{0,7}`).Draw(t, "from"),
					TargetControlID: to + "/" + rapid.StringMatching(`[a-z0-9][a-z0-9.\-]{0,7}`).Draw(t, "to"),
					Relationship:    rapid.SampledFrom(relationships).Draw(t, "relationship"),
					Confidence:      rapid.Float64Range(0, 1).Draw(t, "confidence"),
				})
			}
			return in
		}

		build := func(t *rapid.T, in oscal.MappingExport) []byte {
			out, err := oscal.BuildMappingCollection(in)
			if err != nil {
				t.Fatalf("BuildMappingCollection: %v", err)
			}
			return out
		}
		docUUID := func(t *rapid.T, in oscal.MappingExport) string {
			var top map[string]map[string]any
			if err := json.Unmarshal(build(t, in), &top); err != nil {
				t.Fatalf("output is not JSON: %v", err)
			}
			return top["mapping-collection"]["uuid"].(string)
		}

		It("is deterministic and always yields a schema-valid document", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				in := drawExport(t)
				out := build(t, in)
				if again := build(t, in); string(again) != string(out) {
					t.Fatalf("same input gave different output")
				}
				var model map[string]any
				if err := json.Unmarshal(out, &model); err != nil {
					t.Fatalf("output is not JSON: %v", err)
				}
				v, err := validation.NewValidatorDesiredVersion(model, "1.2.3")
				if err != nil {
					t.Fatalf("NewValidatorDesiredVersion: %v", err)
				}
				if err := v.Validate(); err != nil {
					t.Fatalf("output fails OSCAL 1.2.3 validation: %v", err)
				}
			})
		})

		It("changes the document uuid when one row's confidence changes", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				in := drawExport(t)
				before := docUUID(t, in)
				i := rapid.IntRange(0, len(in.Mappings)-1).Draw(t, "row")
				old := in.Mappings[i].Confidence
				in.Mappings[i].Confidence = math.Mod(old+0.25, 1.0001)
				if after := docUUID(t, in); after == before {
					t.Fatalf("uuid unchanged after confidence %v -> %v", old, in.Mappings[i].Confidence)
				}
			})
		})

		It("changes the document uuid when one row's relationship changes", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				in := drawExport(t)
				before := docUUID(t, in)
				i := rapid.IntRange(0, len(in.Mappings)-1).Draw(t, "row")
				old := in.Mappings[i].Relationship
				in.Mappings[i].Relationship = rapid.SampledFrom(relationships).Filter(func(r string) bool { return r != old }).Draw(t, "newRelationship")
				if after := docUUID(t, in); after == before {
					t.Fatalf("uuid unchanged after relationship %s -> %s", old, in.Mappings[i].Relationship)
				}
			})
		})
	})
})
