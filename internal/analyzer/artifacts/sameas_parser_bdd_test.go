package artifacts_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
)

var _ = Describe("ParseSameAsResponse", func() {
	yes, no := true, false

	DescribeTable("reads the decision, justification and confidence",
		func(raw string, decision *bool, justification, confidence string) {
			v := artifacts.ParseSameAsResponse("m1__s0", raw)
			Expect(v.VoterID).To(Equal("m1__s0"))
			Expect(v.RawResponse).To(Equal(raw))
			if decision == nil {
				Expect(v.Decision).To(BeNil())
			} else {
				Expect(v.Decision).To(HaveValue(Equal(*decision)))
			}
			Expect(v.Justification).To(Equal(justification))
			Expect(v.Confidence).To(Equal(confidence))
		},
		Entry("canonical YES", "SAME: YES\nJUSTIFICATION: One document.\nCONFIDENCE: HIGH", &yes, "One document.", "HIGH"),
		Entry("lower-case NO", "same: no\njustification: Narrower scope.\nconfidence: medium", &no, "Narrower scope.", "MEDIUM"),
		Entry("indented line, no confidence", "  SAME: Yes", &yes, "", "LOW"),
		Entry("trailing punctuation", "SAME: YES.\nCONFIDENCE: LOW", &yes, "", "LOW"),
		Entry("first SAME line wins", "SAME: NO\nSAME: YES", &no, "", "LOW"),
		Entry("invalid value", "SAME: MAYBE\nJUSTIFICATION: unsure", nil, "", "LOW"),
		Entry("word that starts with YES", "SAME: YESTERDAY", nil, "", "LOW"),
		Entry("SAME: not at a line start", "They are the SAME: YES", nil, "", "LOW"),
		Entry("empty reply", "", nil, "", "LOW"),
		Entry("echoed template line", "SAME: YES or NO\nJUSTIFICATION: <one to two sentences>", nil, "", "LOW"),
		Entry("decision on the next line", "SAME:\nNO", nil, "", "LOW"),
		Entry("echoed template line, then an answer", "SAME: YES or NO\nSAME: NO", &no, "", "LOW"),
		Entry("CRLF line endings", "SAME: YES\r\nCONFIDENCE: HIGH\r\n", &yes, "", "HIGH"),
		Entry("trailing blanks", "SAME: NO \t\nCONFIDENCE: HIGH", &no, "", "HIGH"),
	)
})
