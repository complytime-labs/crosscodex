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
})
