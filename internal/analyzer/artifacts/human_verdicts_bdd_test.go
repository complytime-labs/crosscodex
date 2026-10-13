//go:build !integration

package artifacts_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
)

var _ = Describe("PairRef", func() {
	It("is the first 16 hex characters of sha256(pair key)", func() {
		key := artifacts.PairKey("g-a", "g-b")
		sum := sha256.Sum256([]byte(key))
		Expect(artifacts.PairRef(key)).To(Equal(hex.EncodeToString(sum[:])[:16]))
	})
	It("differs for different pairs", func() {
		Expect(artifacts.PairRef(artifacts.PairKey("a", "b"))).NotTo(Equal(artifacts.PairRef(artifacts.PairKey("a", "c"))))
	})
	It("passes ValidatePairRef", func() {
		Expect(artifacts.ValidatePairRef(artifacts.PairRef(artifacts.PairKey("a", "b")))).To(Succeed())
	})
})

var _ = Describe("ValidatePairRef", func() {
	DescribeTable("rejects with an actionable message",
		func(ref string) {
			Expect(artifacts.ValidatePairRef(ref)).To(MatchError(ContainSubstring("copy the PAIR column from the review listing")))
		},
		Entry("empty", ""),
		Entry("too short", "abc123"),
		Entry("too long", strings.Repeat("a", 17)),
		Entry("uppercase hex", strings.Repeat("A", 16)),
		Entry("non-hex", strings.Repeat("g", 16)),
		Entry("a pair key fragment", "artifact-pair|a|b"),
	)
})

var _ = Describe("ValidateReviewer", func() {
	DescribeTable("accepts",
		func(id string) { Expect(artifacts.ValidateReviewer(id)).To(Succeed()) },
		Entry("an email", "alice@example.com"),
		Entry("a name with spaces", "Alice Smith"),
		Entry("exactly 256 bytes", strings.Repeat("a", 256)),
	)
	DescribeTable("rejects with an actionable message",
		func(id, want string) {
			Expect(artifacts.ValidateReviewer(id)).To(MatchError(ContainSubstring(want)))
		},
		Entry("empty", "", "pass the ID of the person recording this verdict"),
		Entry("blank", "   ", "pass the ID of the person recording this verdict"),
		Entry("257 bytes", strings.Repeat("a", 257), "at most 256 bytes"),
		Entry("invalid UTF-8", "al\xffice", "valid UTF-8"),
		Entry("a newline", "alice\nbob", "control, format or line-separator characters"),
		Entry("an ANSI escape", "alice\x1b[31m", "control, format or line-separator characters"),
		Entry("a bidi override", "alice\u202e", "control, format or line-separator characters"),
		Entry("a line separator", "alice\u2028bob", "control, format or line-separator characters"),
		Entry("a paragraph separator", "alice\u2029bob", "control, format or line-separator characters"),
		Entry("a leading space", " alice", "leading or trailing spaces"),
		Entry("a trailing space", "alice ", "leading or trailing spaces"),
		Entry("a trailing no-break space", "alice\u00a0", "leading or trailing spaces"),
	)
})

var _ = Describe("CleanDisplay", func() {
	DescribeTable("makes untrusted text safe for one terminal cell",
		func(in, want string) { Expect(artifacts.CleanDisplay(in, 40)).To(Equal(want)) },
		Entry("plain", "Access Policy", "Access Policy"),
		Entry("escape sequence", "evil\x1b]0;pwned\x07name", "evil ]0;pwned name"),
		Entry("newlines and tabs", "a\n\tb", "a b"),
		Entry("bidi and zero-width removed", "a\u202eb\u200bc", "abc"),
		Entry("invalid UTF-8 repaired", "a\xffb", "a�b"),
		Entry("trimmed", "  a  ", "a"),
		Entry("capped on a rune boundary", strings.Repeat("é", 30), strings.Repeat("é", 20)),
		Entry("keeps < and > (no entity encoding)", "a<b>", "a<b>"),
	)
})
