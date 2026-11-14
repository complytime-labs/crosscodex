package crosswalk_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/crosswalk"
)

var _ = Describe("LineageSegment", func() {
	DescribeTable("renders one ancestor",
		func(title, label, identifier, want string) {
			Expect(crosswalk.LineageSegment(title, label, identifier)).To(Equal(want))
		},
		Entry("title and label", "Account Management", "AC-2", "ac-2", "Account Management (AC-2)"),
		Entry("label preferred over identifier", "Privileged User Accounts", "AC-2(7)", "ac-2.7", "Privileged User Accounts (AC-2(7))"),
		Entry("identifier when the label is empty", "Planning", "", "pl", "Planning (pl)"),
		Entry("identifier when the label is whitespace only", "Planning", " \t", "pl", "Planning (pl)"),
		Entry("title alone when neither is present", "Guidelines", "", "", "Guidelines"),
		Entry("title alone when both are whitespace only", "Guidelines", " ", "\n", "Guidelines"),
		Entry("empty for an empty title", "", "AC", "ac", ""),
		Entry("empty for a whitespace-only title", "  \t", "AC", "ac", ""),
		Entry("trimmed title", "  Access Control ", "AC", "ac", "Access Control (AC)"),
	)
})
