package oscal_test

import (
	"strings"
	"unicode"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

// Suite bootstrap lives in oscal_bdd_test.go (TestOscalBDD).
// This file only registers Describe nodes; Ginkgo collects them automatically.

var _ = Describe("Property Specifications", func() {
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
})
