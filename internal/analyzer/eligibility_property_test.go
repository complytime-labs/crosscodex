//go:build !integration

package analyzer_test

import (
	"strings"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/internal/analyzer"
)

var _ = Describe("Property Specifications", Ordered, func() {
	Context("PromptContext", func() {
		It("never exceeds the byte bound, never splits a rune, and keeps the last segment when it fits", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				segs := rapid.SliceOfN(rapid.StringMatching(`[A-Za-z0-9é ]{1,12}`), 1, 6).Draw(t, "segments")
				maxBytes := rapid.IntRange(0, 80).Draw(t, "maxBytes")
				got := analyzer.PromptContext(strings.Join(segs, " > "), maxBytes)
				if len(got) > maxBytes {
					t.Fatalf("len %d > bound %d: %q", len(got), maxBytes, got)
				}
				if !utf8.ValidString(got) {
					t.Fatalf("invalid UTF-8: %q", got)
				}
				last := analyzer.PromptValue(segs[len(segs)-1], analyzer.MaxPromptValueBytes*100)
				if len(last) <= maxBytes && !strings.HasSuffix(got, last) {
					t.Fatalf("%q does not end with last segment %q", got, last)
				}
			})
		})
	})

	Context("CleanExport", func() {
		It("returns valid UTF-8 within the bound with nothing from the strip set", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				parts := rapid.SliceOf(rapid.OneOf(
					rapid.String(),
					rapid.SampledFrom([]string{"\xff", "\x1b", "\x7f", "\u009b", "\u202e", "\u2067", "\U000E0041", "\r\n", "\r", "\u200d", "\u200c"}),
				)).Draw(t, "parts")
				maxBytes := rapid.IntRange(0, 80).Draw(t, "maxBytes")
				got := analyzer.CleanExport(strings.Join(parts, ""), maxBytes)
				if !utf8.ValidString(got) || len(got) > maxBytes {
					t.Fatalf("CleanExport = %q: valid=%v len=%d bound=%d", got, utf8.ValidString(got), len(got), maxBytes)
				}
				for _, r := range got {
					if isExportStripped(r) {
						t.Fatalf("CleanExport = %q contains %U", got, r)
					}
				}
			})
		})

		It("keeps joiners and line breaks inside the text", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				keep := []string{"\u200d", "\u200c", "\u200f", "\n", "\t"}
				mid := rapid.SliceOfN(rapid.SampledFrom(append([]string{"a", "\x1b", "\u202e"}, keep...)), 0, 20).Draw(t, "mid")
				in := "x" + strings.Join(mid, "") + "y"
				got := analyzer.CleanExport(in, len(in))
				for _, k := range keep {
					if strings.Count(got, k) != strings.Count(in, k) {
						t.Fatalf("CleanExport(%q) = %q changed the count of %q", in, got, k)
					}
				}
			})
		})
	})
})
