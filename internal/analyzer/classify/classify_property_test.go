package classify_test

import (
	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/internal/analyzer/classify"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
)

var _ = Describe("Property Specifications", Ordered, func() {
	Context("ParseClassification -- never panics", func() {
		It("handles arbitrary string input without panic", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				input := rapid.String().Draw(t, "input")
				// Must not panic -- error is acceptable.
				_, _ = classify.ParseClassification(input)
			})
		})
	})

	Context("ParseClassification -- always valid enums", func() {
		It("returns valid Type and Level for non-empty input", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				input := rapid.StringMatching(`.+`).Draw(t, "input")
				result, err := classify.ParseClassification(input)
				if err != nil {
					return // empty-after-strip case
				}
				if !result.Type.Valid() {
					t.Fatalf("invalid Type %d for input %q", result.Type, input)
				}
				if !result.Level.Valid() {
					t.Fatalf("invalid Level %d for input %q", result.Level, input)
				}
			})
		})
	})

	Context("ParseClassification -- None|None invariant", func() {
		It("forces level to None when type is None", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				// Generate inputs that contain "none" to trigger TypeNone
				prefix := rapid.String().Draw(t, "prefix")
				suffix := rapid.String().Draw(t, "suffix")
				input := prefix + "none" + suffix
				result, err := classify.ParseClassification(input)
				if err != nil {
					return
				}
				if result.Type == classify.TypeNone && result.Level != classify.LevelNone {
					t.Fatalf("None type with non-None level %s for input %q", result.Level, input)
				}
			})
		})
	})

	Context("ParseClassification -- roundtrip", func() {
		It("parses the output of Result.String() back to the same values", func() {
			combos := classify.ValidCombinations()

			rapid.Check(GinkgoT(), func(t *rapid.T) {
				idx := rapid.IntRange(0, len(combos)-1).Draw(t, "comboIdx")
				original := combos[idx]

				serialized := original.String()
				parsed, err := classify.ParseClassification(serialized)
				if err != nil {
					t.Fatalf("roundtrip parse failed for %q: %v", serialized, err)
				}
				if parsed.Type != original.Type {
					t.Fatalf("roundtrip Type mismatch: %s != %s (from %q)", parsed.Type, original.Type, serialized)
				}
				if parsed.Level != original.Level {
					t.Fatalf("roundtrip Level mismatch: %s != %s (from %q)", parsed.Level, original.Level, serialized)
				}
			})
		})
	})

	Context("ParseClassification -- idempotent", func() {
		It("parsing the String of a parsed result yields the same result", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				input := rapid.StringMatching(`.+`).Draw(t, "input")
				first, err := classify.ParseClassification(input)
				if err != nil {
					return
				}
				second, err := classify.ParseClassification(first.String())
				if err != nil {
					t.Fatalf("idempotent parse failed for %q -> %q: %v", input, first.String(), err)
				}
				if first.Type != second.Type || first.Level != second.Level {
					t.Fatalf("not idempotent: %s != %s (from %q)", first, second, input)
				}
			})
		})
	})

	Context("llmclient.ContentHash -- determinism", func() {
		It("same messages always produce the same hash", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				role := rapid.SampledFrom([]string{"system", "user", "assistant"}).Draw(t, "role")
				content := rapid.String().Draw(t, "content")
				msgs := []llmclient.ChatMessage{{Role: role, Content: content}}
				hash1 := llmclient.ContentHash(msgs)
				hash2 := llmclient.ContentHash(msgs)
				if hash1 != hash2 {
					t.Fatalf("non-deterministic hash: %q != %q for %+v", hash1, hash2, msgs)
				}
			})
		})
	})
})
