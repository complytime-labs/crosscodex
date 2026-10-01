package requires_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/requires"
)

var _ = Describe("ParseResponse", func() {
	It("parses YES decisions", func() {
		raw := "REQUIRES: YES\nJUSTIFICATION: needs the target first\nCONFIDENCE: HIGH"
		vote := requires.ParseResponse("voter-1", raw)
		Expect(vote.Decision).NotTo(BeNil())
		Expect(*vote.Decision).To(BeTrue())
		Expect(vote.Confidence).To(Equal("HIGH"))
		Expect(vote.Justification).To(Equal("needs the target first"))
	})

	It("parses NO decisions", func() {
		vote := requires.ParseResponse("voter-1", "REQUIRES: NO\nCONFIDENCE: MEDIUM")
		Expect(vote.Decision).NotTo(BeNil())
		Expect(*vote.Decision).To(BeFalse())
	})

	It("returns a nil Decision for unparseable responses", func() {
		vote := requires.ParseResponse("voter-1", "the model rambled without the expected format")
		Expect(vote.Decision).To(BeNil())
	})

	It("defaults confidence to LOW when absent", func() {
		vote := requires.ParseResponse("voter-1", "REQUIRES: YES")
		Expect(vote.Confidence).To(Equal("LOW"), "default when no CONFIDENCE line present")
	})
})
