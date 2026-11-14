package critique_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
	"github.com/complytime-labs/crosscodex/internal/analyzer/critique"
)

var _ = Describe("ParseCritique", func() {
	DescribeTable("reads a valid vote",
		func(raw, verdict, reason string) {
			c := critique.ParseCritique(raw)
			Expect(c.Valid()).To(BeTrue())
			Expect(c.Verdict).To(Equal(verdict))
			Expect(c.Reason).To(Equal(reason))
		},
		Entry("UPHOLD", "VERDICT: UPHOLD\nREASON: Both require an asset inventory.", critique.VerdictUphold, "Both require an asset inventory."),
		Entry("REJECT", "VERDICT: REJECT\nREASON: They only share a topic.", critique.VerdictReject, "They only share a topic."),
		Entry("lowercase keys and value", "verdict: reject\nreason: no shared obligation", critique.VerdictReject, "no shared obligation"),
		Entry("padded value and CRLF line endings", "VERDICT:  Uphold \r\nREASON: same duty\r\n", critique.VerdictUphold, "same duty"),
		Entry("a repeated identical VERDICT line", "VERDICT: REJECT\nVERDICT: reject\nREASON: r", critique.VerdictReject, "r"),
	)

	DescribeTable("treats anything else as an invalid vote",
		func(raw string) {
			Expect(critique.ParseCritique(raw).Valid()).To(BeFalse())
		},
		Entry("a missing VERDICT line", "REASON: looks fine"),
		Entry("VERDICT: MAYBE", "VERDICT: MAYBE\nREASON: unsure"),
		Entry("a missing REASON line", "VERDICT: UPHOLD"),
		Entry("an empty REASON", "VERDICT: UPHOLD\nREASON:   "),
		Entry("VERDICT lines that disagree", "VERDICT: UPHOLD\nVERDICT: REJECT\nREASON: r"),
		Entry("a verdict followed by more words", "VERDICT: UPHOLD because it fits\nREASON: r"),
		Entry("an empty response", ""),
	)

	It("keeps the reason of an invalid vote", func() {
		Expect(critique.ParseCritique("VERDICT: MAYBE\nREASON: unsure").Reason).To(Equal("unsure"))
	})

	It("cuts a REASON over the storage bound on a rune boundary", func() {
		c := critique.ParseCritique("VERDICT: REJECT\nREASON: " + strings.Repeat("é", intanalyzer.MaxStoredJustificationBytes))
		Expect(c.Valid()).To(BeTrue())
		Expect(c.Reason).To(Equal(strings.Repeat("é", intanalyzer.MaxStoredJustificationBytes/2)))
	})

	It("stores the reason as cleaned raw text: no NUL or invalid UTF-8, and nothing escaped", func() {
		c := critique.ParseCritique("VERDICT: REJECT\nREASON: </justification> a & b\x00\xff \r\n")
		Expect(c.Reason).To(Equal("</justification> a & b\uFFFD"))
	})
})
