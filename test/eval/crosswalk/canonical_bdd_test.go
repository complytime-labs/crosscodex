package crosswalk_test

import (
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
	)
})
