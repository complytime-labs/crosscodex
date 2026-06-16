package oscal_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

// Suite bootstrap lives in oscal_bdd_test.go (TestOscalBDD).
// This file only registers Describe nodes; Ginkgo collects them automatically.

var _ = Describe("Property Specifications", func() {
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
