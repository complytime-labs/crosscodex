package crosswalk_test

import (
	"unicode"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/test/eval/crosswalk"
)

var _ = Describe("Canonical", func() {
	DescribeTable("normalises identifiers to letter and digit runs",
		func(input, want string) {
			Expect(crosswalk.Canonical(input)).To(Equal(want))
		},
		Entry("padded enhancement", "SA-15(07)", "sa.15.7"),
		Entry("unpadded enhancement", "SA-15(7)", "sa.15.7"),
		Entry("dotted lowercase", "sa-15.7", "sa.15.7"),
		Entry("padded base control", "AC-02", "ac.2"),
		Entry("all-numeric padded", "03.01.01", "3.1.1"),
		Entry("letters and digits dotted", "PO.1.1", "po.1.1"),
		Entry("surrounding space and repeated separators", "  x--0 ", "x.0"),
		Entry("only separators", "()", ""),
		Entry("empty", "", ""),
		Entry("letter-digit adjacency splits", "ab12cd", "ab.12.cd"),
		Entry("fullwidth digit", "AC-\uff12", "ac.2"),
		Entry("fullwidth padded", "AC-\uff10\uff12", "ac.2"),
		Entry("Arabic-Indic digits", "AC-\u0661\u0662", "ac.12"),
		Entry("Arabic-Indic padded", "AC-\u0660\u0662", "ac.2"),
		Entry("Devanagari digit", "SA-\u096d", "sa.7"),
		Entry("fullwidth letters", "\uff21\uff23-2", "ac.2"),
		Entry("mixed script digits in one run", "x\u0661\uff12\u0663", "x.123"),
		Entry("invalid byte before a compatibility letter", "\xf3\u03f0", "\u03ba"),
		Entry("invalid byte between letters is a separator", "a\xffb", "a.b"),
	)

	It("is idempotent on input with invalid UTF-8", func() {
		once := crosswalk.Canonical("\xf3\u03f0")
		Expect(crosswalk.Canonical(once)).To(Equal(once))
	})

	It("maps every Unicode decimal digit to the ASCII digit of the same value", func() {
		zeros := 0
		for r := rune(0); r <= unicode.MaxRune; r++ {
			if !unicode.IsDigit(r) {
				continue
			}
			got := crosswalk.Canonical("a" + string(r))
			Expect(got).To(MatchRegexp(`^a\.[0-9]$`), "U+%04X", r)
			if got == "a.0" {
				zeros++
			}
		}
		Expect(zeros).To(BeNumerically(">", 50), "each Nd block has exactly one zero")
	})
})
